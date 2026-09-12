package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/queue"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/shridarpatil/whatomate/internal/utils"
	"github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/internal/whatsappaccount"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CampaignRequest represents campaign create/update request
type CampaignRequest struct {
	Name            string     `json:"name" validate:"required"`
	WhatsAppAccount string     `json:"whatsapp_account" validate:"required"`
	TemplateID      string     `json:"template_id" validate:"required"`
	HeaderMediaID   string     `json:"header_media_id"`
	ScheduledAt     *time.Time `json:"scheduled_at"`
}

type campaignHandlerError struct {
	status    int
	message   string
	operation string
	cause     error
}

func (e *campaignHandlerError) Error() string {
	if e == nil {
		return "campaign operation failed"
	}
	if e.cause != nil {
		return fmt.Sprintf("%s: %v", e.operation, e.cause)
	}
	return e.operation
}

func (e *campaignHandlerError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

var errCampaignMediaCandidateReferenced = errors.New("campaign media candidate is already referenced")

var (
	errCampaignTemplateNotFound        = errors.New("campaign template not found")
	errCampaignTemplateAccountMismatch = errors.New("campaign template does not belong to WhatsApp account")
	errCampaignTemplateNotApproved     = errors.New("campaign template is not approved")
	errCampaignAccountUnavailable      = errors.New("WhatsApp account is not active")
)

func lockCampaignOutboundAuthority(
	tx *gorm.DB,
	organizationID, templateID uuid.UUID,
	accountName string,
	requireApproved bool,
) (*models.Template, error) {
	var template models.Template
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).
		Where("id = ? AND organization_id = ?", templateID, organizationID).
		First(&template).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errCampaignTemplateNotFound
		}
		return nil, fmt.Errorf("lock campaign template: %w", err)
	}
	if template.WhatsAppAccount != accountName {
		return nil, errCampaignTemplateAccountMismatch
	}
	if requireApproved && template.Status != string(models.TemplateStatusApproved) {
		return nil, errCampaignTemplateNotApproved
	}

	var accountReference models.WhatsAppAccount
	if err := tx.Select("id").
		Where("name = ? AND organization_id = ?", accountName, organizationID).
		First(&accountReference).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errCampaignAccountUnavailable
		}
		return nil, fmt.Errorf("resolve campaign WhatsApp account: %w", err)
	}
	account, err := whatsappaccount.LockAndLoadActiveForOutbound(tx, organizationID, accountReference.ID)
	if err != nil || account.Name != accountName {
		if err == nil {
			err = errCampaignAccountUnavailable
		}
		return nil, fmt.Errorf("%w: %v", errCampaignAccountUnavailable, err)
	}
	return &template, nil
}

func nextCampaignGeneration(previous *time.Time) time.Time {
	generation := time.Now().UTC().Truncate(time.Microsecond)
	if previous != nil {
		prior := previous.UTC().Truncate(time.Microsecond)
		if !generation.After(prior) {
			generation = prior.Add(time.Microsecond)
		}
	}
	return generation
}

// CampaignResponse represents campaign in API responses
type CampaignResponse struct {
	ID                  uuid.UUID             `json:"id"`
	Name                string                `json:"name"`
	WhatsAppAccount     string                `json:"whatsapp_account"`
	TemplateID          uuid.UUID             `json:"template_id"`
	TemplateName        string                `json:"template_name,omitempty"`
	HeaderMediaID       string                `json:"header_media_id,omitempty"`
	HeaderMediaFilename string                `json:"header_media_filename,omitempty"`
	HeaderMediaMimeType string                `json:"header_media_mime_type,omitempty"`
	Status              models.CampaignStatus `json:"status"`
	TotalRecipients     int                   `json:"total_recipients"`
	SentCount           int                   `json:"sent_count"`
	DeliveredCount      int                   `json:"delivered_count"`
	ReadCount           int                   `json:"read_count"`
	FailedCount         int                   `json:"failed_count"`
	ScheduledAt         *time.Time            `json:"scheduled_at,omitempty"`
	StartedAt           *time.Time            `json:"started_at,omitempty"`
	CompletedAt         *time.Time            `json:"completed_at,omitempty"`
	CreatedByName       string                `json:"created_by_name,omitempty"`
	UpdatedByName       string                `json:"updated_by_name,omitempty"`
	CreatedAt           time.Time             `json:"created_at"`
	UpdatedAt           time.Time             `json:"updated_at"`
}

// RecipientRequest represents recipient import request
type RecipientRequest struct {
	PhoneNumber    string         `json:"phone_number" validate:"required"`
	RecipientName  string         `json:"recipient_name"`
	TemplateParams map[string]any `json:"template_params"`
	// HeaderParams carries the value for a TEXT-header variable (max 1 per
	// Meta), keyed by the variable's name. Kept separate from TemplateParams
	// so a positional header {{1}} doesn't collide with body {{1}}.
	HeaderParams map[string]any `json:"header_params"`
}

// ListCampaigns implements campaign listing
func (a *App) ListCampaigns(r *fastglue.Request) error {
	orgID, err := a.getOrgID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	pg := parsePagination(r)

	// Get query params
	status := string(r.RequestCtx.QueryArgs().Peek("status"))
	whatsappAccount := string(r.RequestCtx.QueryArgs().Peek("whatsapp_account"))
	search := string(r.RequestCtx.QueryArgs().Peek("search"))

	baseQuery := a.DB.Where("organization_id = ?", orgID)

	if search != "" {
		baseQuery = baseQuery.Where("name ILIKE ?", "%"+search+"%")
	}

	if status != "" {
		baseQuery = baseQuery.Where("status = ?", status)
	}
	if whatsappAccount != "" {
		baseQuery = baseQuery.Where("whats_app_account = ?", whatsappAccount)
	}
	if from, ok := parseDateParam(r, "from"); ok {
		baseQuery = baseQuery.Where("created_at >= ?", from)
	}
	if to, ok := parseDateParam(r, "to"); ok {
		baseQuery = baseQuery.Where("created_at <= ?", endOfDay(to))
	}

	// Get total count
	var total int64
	baseQuery.Model(&models.BulkMessageCampaign{}).Count(&total)

	var campaigns []models.BulkMessageCampaign
	if err := pg.Apply(baseQuery.
		Preload("Template").
		Order("created_at DESC")).
		Find(&campaigns).Error; err != nil {
		a.Log.Error("Failed to list campaigns", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list campaigns", nil, "")
	}

	// Convert to response format
	response := make([]CampaignResponse, len(campaigns))
	for i, c := range campaigns {
		response[i] = CampaignResponse{
			ID:                  c.ID,
			Name:                c.Name,
			WhatsAppAccount:     c.WhatsAppAccount,
			TemplateID:          c.TemplateID,
			HeaderMediaID:       c.HeaderMediaID,
			HeaderMediaFilename: c.HeaderMediaFilename,
			HeaderMediaMimeType: c.HeaderMediaMimeType,
			Status:              c.Status,
			TotalRecipients:     c.TotalRecipients,
			SentCount:           c.SentCount,
			DeliveredCount:      c.DeliveredCount,
			ReadCount:           c.ReadCount,
			FailedCount:         c.FailedCount,
			ScheduledAt:         c.ScheduledAt,
			StartedAt:           c.StartedAt,
			CompletedAt:         c.CompletedAt,
			CreatedAt:           c.CreatedAt,
			UpdatedAt:           c.UpdatedAt,
		}
		if c.Template != nil {
			response[i].TemplateName = c.Template.Name
		}
	}

	return r.SendEnvelope(listEnvelope("campaigns", response, total, pg))
}

// CreateCampaign implements campaign creation
func (a *App) CreateCampaign(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	var req CampaignRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	templateID, err := uuid.Parse(req.TemplateID)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid template ID", nil, "")
	}

	campaign := models.BulkMessageCampaign{
		OrganizationID:  orgID,
		WhatsAppAccount: req.WhatsAppAccount,
		Name:            req.Name,
		TemplateID:      templateID,
		HeaderMediaID:   req.HeaderMediaID,
		Status:          models.CampaignStatusDraft,
		ScheduledAt:     req.ScheduledAt,
		CreatedBy:       userID,
		UpdatedByID:     &userID,
	}
	var template *models.Template
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var authorityErr error
		template, authorityErr = lockCampaignOutboundAuthority(
			scoped.DB,
			orgID,
			templateID,
			req.WhatsAppAccount,
			false,
		)
		if authorityErr != nil {
			return authorityErr
		}
		if createErr := scoped.DB.Create(&campaign).Error; createErr != nil {
			return fmt.Errorf("create campaign: %w", createErr)
		}
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, errCampaignTemplateNotFound):
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Template not found", nil, "")
		case errors.Is(err, errCampaignTemplateAccountMismatch):
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, errCampaignTemplateAccountMismatch.Error(), nil, "")
		case errors.Is(err, errCampaignAccountUnavailable):
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "WhatsApp account not found", nil, "")
		default:
			a.Log.Error("Failed to create campaign", "error", err)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create campaign", nil, "")
		}
	}

	a.logAudit(orgID, userID,
		"campaign", campaign.ID, models.AuditActionCreated, nil, &campaign)

	a.Log.Info("Campaign created", "campaign_id", campaign.ID, "name", campaign.Name)

	return r.SendEnvelope(CampaignResponse{
		ID:                  campaign.ID,
		Name:                campaign.Name,
		WhatsAppAccount:     campaign.WhatsAppAccount,
		TemplateID:          campaign.TemplateID,
		TemplateName:        template.Name,
		HeaderMediaID:       campaign.HeaderMediaID,
		HeaderMediaFilename: campaign.HeaderMediaFilename,
		HeaderMediaMimeType: campaign.HeaderMediaMimeType,
		Status:              campaign.Status,
		TotalRecipients:     campaign.TotalRecipients,
		SentCount:           campaign.SentCount,
		DeliveredCount:      campaign.DeliveredCount,
		ReadCount:           campaign.ReadCount,
		FailedCount:         campaign.FailedCount,
		ScheduledAt:         campaign.ScheduledAt,
		CreatedAt:           campaign.CreatedAt,
		UpdatedAt:           campaign.UpdatedAt,
	})
}

// GetCampaign implements getting a single campaign
func (a *App) GetCampaign(r *fastglue.Request) error {
	orgID, err := a.getOrgID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	id, err := parsePathUUID(r, "id", "campaign")
	if err != nil {
		return nil
	}

	var campaign models.BulkMessageCampaign
	if err := a.DB.Where("id = ? AND organization_id = ?", id, orgID).
		Preload("Template").
		Preload("Creator").
		Preload("UpdatedBy").
		First(&campaign).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Campaign not found", nil, "")
	}

	response := CampaignResponse{
		ID:                  campaign.ID,
		Name:                campaign.Name,
		WhatsAppAccount:     campaign.WhatsAppAccount,
		TemplateID:          campaign.TemplateID,
		HeaderMediaID:       campaign.HeaderMediaID,
		HeaderMediaFilename: campaign.HeaderMediaFilename,
		HeaderMediaMimeType: campaign.HeaderMediaMimeType,
		Status:              campaign.Status,
		TotalRecipients:     campaign.TotalRecipients,
		SentCount:           campaign.SentCount,
		DeliveredCount:      campaign.DeliveredCount,
		ReadCount:           campaign.ReadCount,
		FailedCount:         campaign.FailedCount,
		ScheduledAt:         campaign.ScheduledAt,
		StartedAt:           campaign.StartedAt,
		CompletedAt:         campaign.CompletedAt,
		CreatedAt:           campaign.CreatedAt,
		UpdatedAt:           campaign.UpdatedAt,
	}
	if campaign.Template != nil {
		response.TemplateName = campaign.Template.Name
	}
	if campaign.Creator != nil {
		response.CreatedByName = campaign.Creator.FullName
	}
	if campaign.UpdatedBy != nil {
		response.UpdatedByName = campaign.UpdatedBy.FullName
	}

	return r.SendEnvelope(response)
}

// UpdateCampaign implements campaign update
func (a *App) UpdateCampaign(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	id, err := parsePathUUID(r, "id", "campaign")
	if err != nil {
		return nil
	}

	var req CampaignRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	var requestedTemplateID uuid.UUID
	if req.TemplateID != "" {
		requestedTemplateID, err = uuid.Parse(req.TemplateID)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid template ID", nil, "")
		}
	}

	var campaign models.BulkMessageCampaign
	var oldCampaign models.BulkMessageCampaign
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		if loadErr := scoped.DB.
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", id, orgID).
			First(&campaign).Error; loadErr != nil {
			status := fasthttp.StatusInternalServerError
			message := "Failed to load campaign"
			if errors.Is(loadErr, gorm.ErrRecordNotFound) {
				status = fasthttp.StatusNotFound
				message = "Campaign not found"
			}
			return &campaignHandlerError{status: status, message: message, operation: "lock campaign for update", cause: loadErr}
		}
		if campaign.Status != models.CampaignStatusDraft {
			return &campaignHandlerError{
				status: fasthttp.StatusBadRequest, message: "Can only update draft campaigns", operation: "validate draft campaign",
			}
		}
		oldCampaign = campaign

		targetTemplateID := campaign.TemplateID
		if requestedTemplateID != uuid.Nil {
			targetTemplateID = requestedTemplateID
		}
		targetAccountName := campaign.WhatsAppAccount
		if req.WhatsAppAccount != "" {
			targetAccountName = req.WhatsAppAccount
		}

		// Campaign -> template -> account is also the media-upload lock order.
		// Holding all three through the guarded update prevents StartCampaign or
		// UploadCampaignMedia from validating one generation and persisting into
		// another.
		var template models.Template
		if loadErr := scoped.DB.
			Clauses(clause.Locking{Strength: "SHARE"}).
			Where("id = ? AND organization_id = ?", targetTemplateID, orgID).
			First(&template).Error; loadErr != nil {
			return &campaignHandlerError{
				status: fasthttp.StatusBadRequest, message: "Campaign template not found", operation: "lock campaign template", cause: loadErr,
			}
		}
		if template.WhatsAppAccount != targetAccountName {
			return &campaignHandlerError{
				status: fasthttp.StatusBadRequest, message: "Campaign template does not belong to WhatsApp account", operation: "validate template account",
			}
		}

		var accountReference models.WhatsAppAccount
		if loadErr := scoped.DB.
			Select("id").
			Where("name = ? AND organization_id = ?", targetAccountName, orgID).
			First(&accountReference).Error; loadErr != nil {
			return &campaignHandlerError{
				status: fasthttp.StatusBadRequest, message: "WhatsApp account not found", operation: "resolve WhatsApp account", cause: loadErr,
			}
		}
		lockedAccount, accountErr := whatsappaccount.LockAndLoadActiveForOutbound(scoped.DB, orgID, accountReference.ID)
		if accountErr != nil || lockedAccount.Name != targetAccountName {
			return &campaignHandlerError{
				status: fasthttp.StatusBadRequest, message: "WhatsApp account not found", operation: "lock WhatsApp account", cause: accountErr,
			}
		}

		updates := map[string]any{
			"name":              req.Name,
			"scheduled_at":      req.ScheduledAt,
			"updated_by_id":     userID,
			"template_id":       targetTemplateID,
			"whats_app_account": targetAccountName,
		}
		if targetTemplateID != campaign.TemplateID || targetAccountName != campaign.WhatsAppAccount {
			// A Meta media ID is scoped to the provider/template generation. Keep
			// the immutable object itself for already-created messages, but do not
			// carry its projection into a different campaign authority.
			updates["header_media_id"] = ""
			updates["header_media_filename"] = ""
			updates["header_media_mime_type"] = ""
			updates["header_media_local_path"] = ""
		}

		updated := scoped.DB.Model(&models.BulkMessageCampaign{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND template_id = ? AND whats_app_account = ?",
				campaign.ID,
				orgID,
				models.CampaignStatusDraft,
				campaign.TemplateID,
				campaign.WhatsAppAccount,
			).
			Updates(updates)
		if updated.Error != nil {
			return &campaignHandlerError{
				status: fasthttp.StatusInternalServerError, message: "Failed to update campaign", operation: "persist campaign update", cause: updated.Error,
			}
		}
		if updated.RowsAffected != 1 {
			return &campaignHandlerError{
				status: fasthttp.StatusConflict, message: "Campaign changed while updating", operation: "persist campaign update",
			}
		}
		if loadErr := scoped.DB.
			Where("id = ? AND organization_id = ?", campaign.ID, orgID).
			Preload("Template").Preload("Creator").Preload("UpdatedBy").
			First(&campaign).Error; loadErr != nil {
			return &campaignHandlerError{
				status: fasthttp.StatusInternalServerError, message: "Failed to update campaign", operation: "reload campaign", cause: loadErr,
			}
		}
		return nil
	})
	if err != nil {
		var handlerErr *campaignHandlerError
		if errors.As(err, &handlerErr) {
			if handlerErr.cause != nil {
				a.Log.Error("Campaign update failed", "operation", handlerErr.operation, "error", handlerErr.cause, "campaign_id", id, "org_id", orgID)
			}
			return r.SendErrorEnvelope(handlerErr.status, handlerErr.message, nil, "")
		}
		a.Log.Error("Campaign update transaction failed", "error", err, "campaign_id", id, "org_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update campaign", nil, "")
	}

	a.logAudit(orgID, userID,
		"campaign", campaign.ID, models.AuditActionUpdated, &oldCampaign, &campaign)

	response := CampaignResponse{
		ID:                  campaign.ID,
		Name:                campaign.Name,
		WhatsAppAccount:     campaign.WhatsAppAccount,
		TemplateID:          campaign.TemplateID,
		HeaderMediaID:       campaign.HeaderMediaID,
		HeaderMediaFilename: campaign.HeaderMediaFilename,
		HeaderMediaMimeType: campaign.HeaderMediaMimeType,
		Status:              campaign.Status,
		TotalRecipients:     campaign.TotalRecipients,
		SentCount:           campaign.SentCount,
		DeliveredCount:      campaign.DeliveredCount,
		ReadCount:           campaign.ReadCount,
		FailedCount:         campaign.FailedCount,
		ScheduledAt:         campaign.ScheduledAt,
		CreatedAt:           campaign.CreatedAt,
		UpdatedAt:           campaign.UpdatedAt,
	}
	if campaign.Template != nil {
		response.TemplateName = campaign.Template.Name
	}
	if campaign.Creator != nil {
		response.CreatedByName = campaign.Creator.FullName
	}
	if campaign.UpdatedBy != nil {
		response.UpdatedByName = campaign.UpdatedBy.FullName
	}

	return r.SendEnvelope(response)
}

// DeleteCampaign implements campaign deletion
func (a *App) DeleteCampaign(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	id, err := parsePathUUID(r, "id", "campaign")
	if err != nil {
		return nil
	}

	var campaign models.BulkMessageCampaign
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		if loadErr := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", id, orgID).
			First(&campaign).Error; loadErr != nil {
			status := fasthttp.StatusInternalServerError
			message := "Failed to load campaign"
			if errors.Is(loadErr, gorm.ErrRecordNotFound) {
				status = fasthttp.StatusNotFound
				message = "Campaign not found"
			}
			return &campaignHandlerError{status: status, message: message, operation: "lock campaign for deletion", cause: loadErr}
		}
		if campaign.Status == models.CampaignStatusProcessing || campaign.Status == models.CampaignStatusQueued {
			return &campaignHandlerError{status: fasthttp.StatusBadRequest, message: "Cannot delete running campaign", operation: "validate campaign deletion"}
		}
		if deleteErr := scoped.DB.Where("campaign_id = ?", id).Delete(&models.BulkMessageRecipient{}).Error; deleteErr != nil {
			return fmt.Errorf("delete campaign recipients: %w", deleteErr)
		}
		result := scoped.DB.Where("id = ? AND organization_id = ?", id, orgID).Delete(&models.BulkMessageCampaign{})
		if result.Error != nil {
			return fmt.Errorf("delete campaign: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return errors.New("campaign changed before deletion")
		}
		return nil
	})
	if err != nil {
		var handlerErr *campaignHandlerError
		if errors.As(err, &handlerErr) {
			return r.SendErrorEnvelope(handlerErr.status, handlerErr.message, nil, "")
		}
		a.Log.Error("Failed to delete campaign", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete campaign", nil, "")
	}

	a.logAudit(orgID, userID,
		"campaign", id, models.AuditActionDeleted, &campaign, nil)

	a.Log.Info("Campaign deleted", "campaign_id", id)

	return r.SendEnvelope(map[string]any{
		"message": "Campaign deleted successfully",
	})
}

// StartCampaign implements starting a campaign
func (a *App) StartCampaign(r *fastglue.Request) error {
	orgID, err := a.getOrgID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	id, err := parsePathUUID(r, "id", "campaign")
	if err != nil {
		return nil
	}

	var campaign models.BulkMessageCampaign
	var recipients []models.BulkMessageRecipient
	var priorStatus models.CampaignStatus
	var generation time.Time
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		if loadErr := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", id, orgID).
			First(&campaign).Error; loadErr != nil {
			status := fasthttp.StatusInternalServerError
			message := "Failed to load campaign"
			if errors.Is(loadErr, gorm.ErrRecordNotFound) {
				status = fasthttp.StatusNotFound
				message = "Campaign not found"
			}
			return &campaignHandlerError{status: status, message: message, operation: "lock campaign for start", cause: loadErr}
		}
		if campaign.Status != models.CampaignStatusDraft && campaign.Status != models.CampaignStatusScheduled && campaign.Status != models.CampaignStatusPaused {
			return &campaignHandlerError{status: fasthttp.StatusBadRequest, message: "Campaign cannot be started in current state", operation: "validate campaign start"}
		}
		if _, authorityErr := lockCampaignOutboundAuthority(scoped.DB, orgID, campaign.TemplateID, campaign.WhatsAppAccount, true); authorityErr != nil {
			message := "Campaign outbound authority is no longer valid"
			if errors.Is(authorityErr, errCampaignTemplateNotFound) {
				message = "Campaign template no longer exists"
			} else if errors.Is(authorityErr, errCampaignTemplateNotApproved) {
				message = errCampaignTemplateNotApproved.Error()
			}
			return &campaignHandlerError{status: fasthttp.StatusBadRequest, message: message, operation: "validate campaign outbound authority", cause: authorityErr}
		}
		if loadErr := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("campaign_id = ? AND status = ?", id, models.MessageStatusPending).
			Find(&recipients).Error; loadErr != nil {
			return fmt.Errorf("load pending campaign recipients: %w", loadErr)
		}
		if len(recipients) == 0 {
			return &campaignHandlerError{status: fasthttp.StatusBadRequest, message: "Campaign has no pending recipients", operation: "validate campaign recipients"}
		}

		priorStatus = campaign.Status
		generation = nextCampaignGeneration(campaign.StartedAt)
		updated := scoped.DB.Model(&models.BulkMessageCampaign{}).
			Where("id = ? AND organization_id = ? AND status = ?", id, orgID, priorStatus).
			Updates(map[string]any{
				"status":       models.CampaignStatusProcessing,
				"started_at":   generation,
				"completed_at": nil,
			})
		if updated.Error != nil {
			return fmt.Errorf("start campaign: %w", updated.Error)
		}
		if updated.RowsAffected != 1 {
			return errors.New("campaign changed before start")
		}
		return nil
	})
	if err != nil {
		var handlerErr *campaignHandlerError
		if errors.As(err, &handlerErr) {
			return r.SendErrorEnvelope(handlerErr.status, handlerErr.message, nil, "")
		}
		a.Log.Error("Failed to start campaign", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to start campaign", nil, "")
	}

	a.Log.Info("Campaign started", "campaign_id", id, "recipients", len(recipients))

	// Enqueue all recipients as individual jobs for parallel processing
	jobs := make([]*queue.RecipientJob, len(recipients))
	for i, recipient := range recipients {
		jobs[i] = &queue.RecipientJob{
			CampaignID:     id,
			RecipientID:    recipient.ID,
			OrganizationID: orgID,
			PhoneNumber:    recipient.PhoneNumber,
			RecipientName:  recipient.RecipientName,
			TemplateParams: recipient.TemplateParams,
			HeaderParams:   recipient.HeaderParams,
			EnqueuedAt:     generation,
		}
	}

	if err := a.Queue.EnqueueRecipients(r.RequestCtx, jobs); err != nil {
		a.Log.Error("Failed to enqueue recipients", "error", err)
		compensationErr := a.WithCommittedTenantApp(orgID, func(scoped *App) error {
			var current models.BulkMessageCampaign
			if lockErr := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND organization_id = ?", id, orgID).
				First(&current).Error; lockErr != nil {
				return lockErr
			}
			if current.Status != models.CampaignStatusProcessing || current.StartedAt == nil || !current.StartedAt.Equal(generation) {
				return nil
			}
			return scoped.DB.Model(&models.BulkMessageCampaign{}).
				Where("id = ? AND organization_id = ? AND status = ? AND started_at = ?", id, orgID, models.CampaignStatusProcessing, generation).
				Update("status", priorStatus).Error
		})
		if compensationErr != nil {
			a.Log.Error("Failed to compensate campaign after queue error", "error", compensationErr, "campaign_id", id)
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to queue recipients", nil, "")
	}

	a.Log.Info("Recipients enqueued for processing", "campaign_id", id, "count", len(jobs))

	return r.SendEnvelope(map[string]any{
		"message": "Campaign started",
		"status":  models.CampaignStatusProcessing,
	})
}

// PauseCampaign implements pausing a campaign
func (a *App) PauseCampaign(r *fastglue.Request) error {
	orgID, err := a.getOrgID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	id, err := parsePathUUID(r, "id", "campaign")
	if err != nil {
		return nil
	}

	campaign, err := findByIDAndOrg[models.BulkMessageCampaign](a.DB, r, id, orgID, "Campaign")
	if err != nil {
		return nil
	}

	if campaign.Status != models.CampaignStatusProcessing && campaign.Status != models.CampaignStatusQueued {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Campaign is not running", nil, "")
	}

	updated := a.DB.Model(&models.BulkMessageCampaign{}).
		Where(
			"id = ? AND organization_id = ? AND status IN ?",
			id,
			orgID,
			[]models.CampaignStatus{
				models.CampaignStatusProcessing,
				models.CampaignStatusQueued,
			},
		).
		Update("status", models.CampaignStatusPaused)
	if updated.Error != nil {
		a.Log.Error("Failed to pause campaign", "error", updated.Error)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to pause campaign", nil, "")
	}
	if updated.RowsAffected != 1 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Campaign is not running", nil, "")
	}

	a.Log.Info("Campaign paused", "campaign_id", id)

	return r.SendEnvelope(map[string]any{
		"message": "Campaign paused",
		"status":  models.CampaignStatusPaused,
	})
}

// CancelCampaign implements cancelling a campaign
func (a *App) CancelCampaign(r *fastglue.Request) error {
	orgID, err := a.getOrgID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	id, err := parsePathUUID(r, "id", "campaign")
	if err != nil {
		return nil
	}

	campaign, err := findByIDAndOrg[models.BulkMessageCampaign](a.DB, r, id, orgID, "Campaign")
	if err != nil {
		return nil
	}

	if campaign.Status == models.CampaignStatusCompleted || campaign.Status == models.CampaignStatusCancelled {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Campaign already finished", nil, "")
	}

	updated := a.DB.Model(&models.BulkMessageCampaign{}).
		Where(
			"id = ? AND organization_id = ? AND status IN ?",
			id,
			orgID,
			[]models.CampaignStatus{
				models.CampaignStatusDraft,
				models.CampaignStatusScheduled,
				models.CampaignStatusQueued,
				models.CampaignStatusProcessing,
				models.CampaignStatusPaused,
				models.CampaignStatusFailed,
			},
		).
		Update("status", models.CampaignStatusCancelled)
	if updated.Error != nil {
		a.Log.Error("Failed to cancel campaign", "error", updated.Error)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to cancel campaign", nil, "")
	}
	if updated.RowsAffected != 1 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Campaign already finished", nil, "")
	}

	a.Log.Info("Campaign cancelled", "campaign_id", id)

	return r.SendEnvelope(map[string]any{
		"message": "Campaign cancelled",
		"status":  models.CampaignStatusCancelled,
	})
}

// RetryFailed retries sending to all failed recipients
func (a *App) RetryFailed(r *fastglue.Request) error {
	orgID, err := a.getOrgID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	id, err := parsePathUUID(r, "id", "campaign")
	if err != nil {
		return nil
	}

	type recipientSnapshot struct {
		ID                uuid.UUID
		MessageID         *uuid.UUID
		WhatsAppMessageID string
		ErrorMessage      string
		SentAt            *time.Time
		DeliveredAt       *time.Time
		ReadAt            *time.Time
	}

	var campaign models.BulkMessageCampaign
	var failedRecipients []models.BulkMessageRecipient
	var snapshots []recipientSnapshot
	var generation time.Time
	var priorStatus models.CampaignStatus
	var priorCompletedAt *time.Time
	var priorSent, priorDelivered, priorRead, priorFailed int
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		if loadErr := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", id, orgID).
			First(&campaign).Error; loadErr != nil {
			status := fasthttp.StatusInternalServerError
			message := "Failed to load campaign"
			if errors.Is(loadErr, gorm.ErrRecordNotFound) {
				status = fasthttp.StatusNotFound
				message = "Campaign not found"
			}
			return &campaignHandlerError{status: status, message: message, operation: "lock campaign for retry", cause: loadErr}
		}
		if campaign.Status != models.CampaignStatusCompleted && campaign.Status != models.CampaignStatusPaused && campaign.Status != models.CampaignStatusFailed {
			return &campaignHandlerError{status: fasthttp.StatusBadRequest, message: "Can only retry failed messages on completed, paused, or failed campaigns", operation: "validate campaign retry"}
		}
		if _, authorityErr := lockCampaignOutboundAuthority(scoped.DB, orgID, campaign.TemplateID, campaign.WhatsAppAccount, true); authorityErr != nil {
			return &campaignHandlerError{status: fasthttp.StatusBadRequest, message: "Campaign outbound authority is no longer valid", operation: "validate campaign retry authority", cause: authorityErr}
		}
		if loadErr := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("campaign_id = ? AND status = ?", id, models.MessageStatusFailed).
			Find(&failedRecipients).Error; loadErr != nil {
			return fmt.Errorf("load failed campaign recipients: %w", loadErr)
		}
		if len(failedRecipients) == 0 {
			return &campaignHandlerError{status: fasthttp.StatusBadRequest, message: "No failed messages to retry", operation: "validate failed campaign recipients"}
		}

		ids := make([]uuid.UUID, len(failedRecipients))
		snapshots = make([]recipientSnapshot, len(failedRecipients))
		for i := range failedRecipients {
			recipient := &failedRecipients[i]
			ids[i] = recipient.ID
			snapshots[i] = recipientSnapshot{
				ID: recipient.ID, MessageID: recipient.MessageID,
				WhatsAppMessageID: recipient.WhatsAppMessageID, ErrorMessage: recipient.ErrorMessage,
				SentAt: recipient.SentAt, DeliveredAt: recipient.DeliveredAt, ReadAt: recipient.ReadAt,
			}
		}
		reset := scoped.DB.Model(&models.BulkMessageRecipient{}).
			Where("campaign_id = ? AND id IN ? AND status = ?", id, ids, models.MessageStatusFailed).
			Updates(map[string]any{
				"status": models.MessageStatusPending, "message_id": nil,
				"whats_app_message_id": "", "error_message": "",
				"sent_at": nil, "delivered_at": nil, "read_at": nil,
			})
		if reset.Error != nil {
			return fmt.Errorf("reset failed campaign recipients: %w", reset.Error)
		}
		if reset.RowsAffected != int64(len(failedRecipients)) {
			return errors.New("failed campaign recipients changed before retry")
		}

		priorStatus = campaign.Status
		priorCompletedAt = campaign.CompletedAt
		priorSent, priorDelivered, priorRead, priorFailed = campaign.SentCount, campaign.DeliveredCount, campaign.ReadCount, campaign.FailedCount
		generation = nextCampaignGeneration(campaign.StartedAt)
		updated := scoped.DB.Model(&models.BulkMessageCampaign{}).
			Where("id = ? AND organization_id = ? AND status = ?", id, orgID, priorStatus).
			Updates(map[string]any{
				"status": models.CampaignStatusProcessing, "started_at": generation,
				"completed_at": nil,
				"failed_count": gorm.Expr("GREATEST(failed_count - ?, 0)", len(failedRecipients)),
			})
		if updated.Error != nil {
			return fmt.Errorf("start campaign retry generation: %w", updated.Error)
		}
		if updated.RowsAffected != 1 {
			return errors.New("campaign changed before retry")
		}
		return nil
	})
	if err != nil {
		var handlerErr *campaignHandlerError
		if errors.As(err, &handlerErr) {
			return r.SendErrorEnvelope(handlerErr.status, handlerErr.message, nil, "")
		}
		a.Log.Error("Failed to prepare campaign retry", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update campaign", nil, "")
	}

	a.Log.Info("Retrying failed messages", "campaign_id", id, "failed_count", len(failedRecipients))

	// Enqueue failed recipients as individual jobs for parallel processing
	jobs := make([]*queue.RecipientJob, len(failedRecipients))
	for i, recipient := range failedRecipients {
		jobs[i] = &queue.RecipientJob{
			CampaignID:     id,
			RecipientID:    recipient.ID,
			OrganizationID: orgID,
			PhoneNumber:    recipient.PhoneNumber,
			RecipientName:  recipient.RecipientName,
			TemplateParams: recipient.TemplateParams,
			HeaderParams:   recipient.HeaderParams,
			EnqueuedAt:     generation,
		}
	}

	if err := a.Queue.EnqueueRecipients(r.RequestCtx, jobs); err != nil {
		a.Log.Error("Failed to enqueue recipients for retry", "error", err)
		compensated := false
		compensationErr := a.WithCommittedTenantApp(orgID, func(scoped *App) error {
			var current models.BulkMessageCampaign
			if lockErr := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND organization_id = ?", id, orgID).
				First(&current).Error; lockErr != nil {
				return lockErr
			}
			if current.Status != models.CampaignStatusProcessing || current.StartedAt == nil || !current.StartedAt.Equal(generation) {
				return nil
			}

			ids := make([]uuid.UUID, len(snapshots))
			for i := range snapshots {
				ids[i] = snapshots[i].ID
			}
			var currentRecipients []models.BulkMessageRecipient
			if loadErr := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("campaign_id = ? AND id IN ?", id, ids).
				Find(&currentRecipients).Error; loadErr != nil {
				return loadErr
			}
			if len(currentRecipients) != len(snapshots) {
				return nil
			}
			for i := range currentRecipients {
				recipient := &currentRecipients[i]
				if recipient.Status != models.MessageStatusPending || recipient.MessageID != nil ||
					recipient.WhatsAppMessageID != "" || recipient.ErrorMessage != "" ||
					recipient.SentAt != nil || recipient.DeliveredAt != nil || recipient.ReadAt != nil {
					return nil
				}
			}
			for i := range snapshots {
				snapshot := &snapshots[i]
				restored := scoped.DB.Model(&models.BulkMessageRecipient{}).
					Where("id = ? AND campaign_id = ? AND status = ? AND message_id IS NULL", snapshot.ID, id, models.MessageStatusPending).
					Updates(map[string]any{
						"status": models.MessageStatusFailed, "message_id": snapshot.MessageID,
						"whats_app_message_id": snapshot.WhatsAppMessageID, "error_message": snapshot.ErrorMessage,
						"sent_at": snapshot.SentAt, "delivered_at": snapshot.DeliveredAt, "read_at": snapshot.ReadAt,
					})
				if restored.Error != nil || restored.RowsAffected != 1 {
					if restored.Error != nil {
						return restored.Error
					}
					return errors.New("campaign retry recipient changed during compensation")
				}
			}
			restored := scoped.DB.Model(&models.BulkMessageCampaign{}).
				Where("id = ? AND organization_id = ? AND status = ? AND started_at = ?", id, orgID, models.CampaignStatusProcessing, generation).
				Updates(map[string]any{
					"status": priorStatus, "completed_at": priorCompletedAt,
					"sent_count": priorSent, "delivered_count": priorDelivered,
					"read_count": priorRead, "failed_count": priorFailed,
				})
			if restored.Error != nil {
				return restored.Error
			}
			if restored.RowsAffected != 1 {
				return errors.New("campaign retry generation changed during compensation")
			}
			compensated = true
			return nil
		})
		if compensationErr != nil {
			a.Log.Error("Failed to compensate campaign retry", "error", compensationErr, "campaign_id", id)
		} else if !compensated {
			a.Log.Warn("Campaign retry was no longer pristine after queue failure; preserving processing generation", "campaign_id", id)
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to queue recipients", nil, "")
	}

	a.Log.Info("Failed recipients enqueued for retry", "campaign_id", id, "count", len(jobs))

	return r.SendEnvelope(map[string]any{
		"message":     "Retrying failed messages",
		"retry_count": len(failedRecipients),
		"status":      models.CampaignStatusProcessing,
	})
}

// ImportRecipients implements adding recipients to a campaign
func (a *App) ImportRecipients(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	id, err := parsePathUUID(r, "id", "campaign")
	if err != nil {
		return nil
	}

	var req struct {
		Recipients []RecipientRequest `json:"recipients" validate:"required"`
	}
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	recipients := make([]models.BulkMessageRecipient, len(req.Recipients))
	for i, rec := range req.Recipients {
		recipients[i] = models.BulkMessageRecipient{
			CampaignID:     id,
			PhoneNumber:    rec.PhoneNumber,
			RecipientName:  rec.RecipientName,
			TemplateParams: models.JSONB(rec.TemplateParams),
			HeaderParams:   models.JSONB(rec.HeaderParams),
			Status:         models.MessageStatusPending,
		}
	}
	var totalCount int64
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var campaign models.BulkMessageCampaign
		if loadErr := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", id, orgID).
			First(&campaign).Error; loadErr != nil {
			status := fasthttp.StatusInternalServerError
			message := "Failed to load campaign"
			if errors.Is(loadErr, gorm.ErrRecordNotFound) {
				status = fasthttp.StatusNotFound
				message = "Campaign not found"
			}
			return &campaignHandlerError{status: status, message: message, operation: "lock campaign for recipient import", cause: loadErr}
		}
		if campaign.Status != models.CampaignStatusDraft {
			return &campaignHandlerError{status: fasthttp.StatusBadRequest, message: "Can only add recipients to draft campaigns", operation: "validate recipient import"}
		}
		if len(recipients) > 0 {
			if createErr := scoped.DB.Create(&recipients).Error; createErr != nil {
				return fmt.Errorf("add campaign recipients: %w", createErr)
			}
		}
		if countErr := scoped.DB.Model(&models.BulkMessageRecipient{}).
			Where("campaign_id = ?", id).Count(&totalCount).Error; countErr != nil {
			return fmt.Errorf("count campaign recipients: %w", countErr)
		}
		updated := scoped.DB.Model(&models.BulkMessageCampaign{}).
			Where("id = ? AND organization_id = ? AND status = ?", id, orgID, models.CampaignStatusDraft).
			Update("total_recipients", totalCount)
		if updated.Error != nil {
			return fmt.Errorf("update campaign recipient count: %w", updated.Error)
		}
		if updated.RowsAffected != 1 {
			return errors.New("campaign changed during recipient import")
		}
		return nil
	})
	if err != nil {
		var handlerErr *campaignHandlerError
		if errors.As(err, &handlerErr) {
			return r.SendErrorEnvelope(handlerErr.status, handlerErr.message, nil, "")
		}
		a.Log.Error("Failed to add recipients", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to add recipients", nil, "")
	}

	a.Log.Info("Recipients added to campaign", "campaign_id", id, "count", len(req.Recipients))

	// Log recipient addition as audit
	phoneNumbers := make([]string, len(req.Recipients))
	for i, rec := range req.Recipients {
		phoneNumbers[i] = rec.PhoneNumber
	}
	a.logAudit(orgID, userID,
		"campaign", id, models.AuditActionUpdated, nil, nil,
		map[string]any{
			"field":     "recipients_added",
			"old_value": nil,
			"new_value": fmt.Sprintf("%d recipients added", len(req.Recipients)),
		})

	return r.SendEnvelope(map[string]any{
		"message":          "Recipients added successfully",
		"added_count":      len(req.Recipients),
		"total_recipients": totalCount,
	})
}

// GetCampaignRecipients implements listing campaign recipients
func (a *App) GetCampaignRecipients(r *fastglue.Request) error {
	orgID, err := a.getOrgID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	id, err := parsePathUUID(r, "id", "campaign")
	if err != nil {
		return nil
	}

	// Verify campaign belongs to org
	_, err = findByIDAndOrg[models.BulkMessageCampaign](a.DB, r, id, orgID, "Campaign")
	if err != nil {
		return nil
	}

	var recipients []models.BulkMessageRecipient
	if err := a.DB.Where("campaign_id = ?", id).Order("created_at ASC").Find(&recipients).Error; err != nil {
		a.Log.Error("Failed to list recipients", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list recipients", nil, "")
	}

	if a.ShouldMaskPhoneNumbers(orgID) {
		for i := range recipients {
			recipients[i].PhoneNumber = utils.MaskPhoneNumber(recipients[i].PhoneNumber)
			recipients[i].RecipientName = utils.MaskIfPhoneNumber(recipients[i].RecipientName)
		}
	}

	return r.SendEnvelope(map[string]any{
		"recipients": recipients,
		"total":      len(recipients),
	})
}

// DeleteCampaignRecipient deletes a single recipient from a campaign
func (a *App) DeleteCampaignRecipient(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	campaignUUID, err := parsePathUUID(r, "id", "campaign")
	if err != nil {
		return nil
	}

	recipientUUID, err := parsePathUUID(r, "recipientId", "recipient")
	if err != nil {
		return nil
	}

	var recipient models.BulkMessageRecipient
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var campaign models.BulkMessageCampaign
		if loadErr := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", campaignUUID, orgID).
			First(&campaign).Error; loadErr != nil {
			status := fasthttp.StatusInternalServerError
			message := "Failed to load campaign"
			if errors.Is(loadErr, gorm.ErrRecordNotFound) {
				status = fasthttp.StatusNotFound
				message = "Campaign not found"
			}
			return &campaignHandlerError{status: status, message: message, operation: "lock campaign for recipient deletion", cause: loadErr}
		}
		if campaign.Status != models.CampaignStatusDraft {
			return &campaignHandlerError{status: fasthttp.StatusBadRequest, message: "Can only delete recipients from draft campaigns", operation: "validate recipient deletion"}
		}
		if loadErr := scoped.DB.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND campaign_id = ?", recipientUUID, campaignUUID).
			First(&recipient).Error; loadErr != nil {
			status := fasthttp.StatusInternalServerError
			message := "Failed to load recipient"
			if errors.Is(loadErr, gorm.ErrRecordNotFound) {
				status = fasthttp.StatusNotFound
				message = "Recipient not found"
			}
			return &campaignHandlerError{status: status, message: message, operation: "lock campaign recipient for deletion", cause: loadErr}
		}
		result := scoped.DB.Where("id = ? AND campaign_id = ?", recipientUUID, campaignUUID).Delete(&models.BulkMessageRecipient{})
		if result.Error != nil {
			return fmt.Errorf("delete campaign recipient: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return errors.New("campaign recipient changed before deletion")
		}
		var totalCount int64
		if countErr := scoped.DB.Model(&models.BulkMessageRecipient{}).
			Where("campaign_id = ?", campaignUUID).Count(&totalCount).Error; countErr != nil {
			return fmt.Errorf("count campaign recipients: %w", countErr)
		}
		updated := scoped.DB.Model(&models.BulkMessageCampaign{}).
			Where("id = ? AND organization_id = ? AND status = ?", campaignUUID, orgID, models.CampaignStatusDraft).
			Update("total_recipients", totalCount)
		if updated.Error != nil {
			return fmt.Errorf("update campaign recipient count: %w", updated.Error)
		}
		if updated.RowsAffected != 1 {
			return errors.New("campaign changed during recipient deletion")
		}
		return nil
	})
	if err != nil {
		var handlerErr *campaignHandlerError
		if errors.As(err, &handlerErr) {
			return r.SendErrorEnvelope(handlerErr.status, handlerErr.message, nil, "")
		}
		a.Log.Error("Failed to delete recipient", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete recipient", nil, "")
	}

	a.logAudit(orgID, userID,
		"campaign", campaignUUID, models.AuditActionUpdated, nil, nil,
		map[string]any{
			"field":     "recipient_removed",
			"old_value": recipient.PhoneNumber,
			"new_value": nil,
		})

	return r.SendEnvelope(map[string]any{
		"message": "Recipient deleted successfully",
	})
}

// UploadCampaignMedia uploads media for a campaign's template header
func (a *App) UploadCampaignMedia(r *fastglue.Request) error {
	orgID, err := a.getOrgID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	campaignUUID, err := parsePathUUID(r, "id", "campaign")
	if err != nil {
		return nil
	}

	// Parse multipart form
	form, err := r.RequestCtx.MultipartForm()
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid multipart form", nil, "")
	}

	files := form.File["file"]
	if len(files) == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "No file provided", nil, "")
	}

	fileHeader := files[0]
	file, err := fileHeader.Open()
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Failed to open file", nil, "")
	}
	defer func() { _ = file.Close() }()

	// Read file content (limit to 16MB)
	const maxMediaSize = 16 << 20 // 16MB
	data, err := io.ReadAll(io.LimitReader(file, maxMediaSize+1))
	if err != nil {
		a.Log.Error("Failed to read file", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to read file", nil, "")
	}
	if len(data) > maxMediaSize {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "File too large. Maximum size is 16MB", nil, "")
	}

	// Determine and validate MIME type
	mimeType := fileHeader.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	allowedMIME := map[string]bool{
		"image/jpeg": true, "image/png": true, "image/webp": true,
		"video/mp4": true, "video/3gpp": true,
		"audio/aac": true, "audio/mp4": true, "audio/mpeg": true, "audio/ogg": true,
		"application/pdf": true, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": true,
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   true,
		"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
	}
	if !allowedMIME[mimeType] {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Unsupported file type: "+mimeType, nil, "")
	}

	ctx := requestContext(r)
	var mediaID string
	var mediaPath string
	var cleanupCreatedCandidate bool
	filename := sanitizeFilename(fileHeader.Filename)
	candidatePath := a.campaignMediaStoragePath(orgID, campaignUUID.String(), data, mimeType)

	// Use a committed inner tenant transaction even when the route itself is
	// tenant-scoped. Lock campaign before account to match the campaign
	// worker's provider fence. A raw commit error is treated as ambiguous and
	// never authorizes object cleanup.
	err = a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var campaign models.BulkMessageCampaign
		if loadErr := scoped.DB.WithContext(ctx).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", campaignUUID, orgID).
			First(&campaign).Error; loadErr != nil {
			status := fasthttp.StatusInternalServerError
			message := "Failed to load campaign"
			if errors.Is(loadErr, gorm.ErrRecordNotFound) {
				status = fasthttp.StatusNotFound
				message = "Campaign not found"
			}
			return &campaignHandlerError{status: status, message: message, operation: "load campaign", cause: loadErr}
		}
		if campaign.Status != models.CampaignStatusDraft {
			return &campaignHandlerError{
				status:    fasthttp.StatusBadRequest,
				message:   "Can only upload media for draft campaigns",
				operation: "validate draft campaign",
			}
		}

		var template models.Template
		if loadErr := scoped.DB.WithContext(ctx).
			Clauses(clause.Locking{Strength: "SHARE"}).
			Where("id = ? AND organization_id = ?", campaign.TemplateID, orgID).
			First(&template).Error; loadErr != nil {
			return &campaignHandlerError{
				status:    fasthttp.StatusBadRequest,
				message:   "Template does not have a media header",
				operation: "load campaign template",
				cause:     loadErr,
			}
		}
		if template.WhatsAppAccount != campaign.WhatsAppAccount {
			return &campaignHandlerError{
				status:    fasthttp.StatusBadRequest,
				message:   "Campaign template does not belong to WhatsApp account",
				operation: "validate campaign template account",
			}
		}
		if template.Status != string(models.TemplateStatusApproved) {
			return &campaignHandlerError{
				status:    fasthttp.StatusBadRequest,
				message:   "Campaign template is not approved",
				operation: "validate campaign template status",
			}
		}
		if !campaignTemplateAllowsMediaMIME(template.HeaderType, mimeType) {
			return &campaignHandlerError{
				status:    fasthttp.StatusBadRequest,
				message:   "File type does not match campaign template media header",
				operation: "validate campaign template",
			}
		}

		var accountReference models.WhatsAppAccount
		if loadErr := scoped.DB.WithContext(ctx).
			Select("id").
			Where("name = ? AND organization_id = ?", campaign.WhatsAppAccount, orgID).
			First(&accountReference).Error; loadErr != nil {
			return &campaignHandlerError{
				status:    fasthttp.StatusBadRequest,
				message:   "WhatsApp account not found",
				operation: "resolve WhatsApp account",
				cause:     loadErr,
			}
		}
		lockedAccount, accountErr := whatsappaccount.LockAndLoadActiveForOutbound(
			scoped.DB.WithContext(ctx),
			orgID,
			accountReference.ID,
		)
		if accountErr != nil || lockedAccount.Name != campaign.WhatsAppAccount {
			return &campaignHandlerError{
				status:    fasthttp.StatusBadRequest,
				message:   "WhatsApp account not found",
				operation: "lock WhatsApp account",
				cause:     accountErr,
			}
		}
		if accountErr = scoped.prepareWhatsAppAccountForRuntime(lockedAccount); accountErr != nil {
			return &campaignHandlerError{
				status:    fasthttp.StatusBadRequest,
				message:   "WhatsApp account not found",
				operation: "prepare WhatsApp account",
				cause:     accountErr,
			}
		}

		mediaPath = candidatePath
		existingData, existingMIME, existingErr := scoped.loadTenantMedia(ctx, orgID, candidatePath)
		switch {
		case existingErr == nil:
			// A digest path can be an older, still-referenced campaign preview.
			// Reuse it byte-for-byte and never claim cleanup ownership.
			objectStoreMIME := scoped.Config != nil && strings.EqualFold(scoped.Config.Storage.Type, "s3")
			if !bytes.Equal(existingData, data) || (objectStoreMIME && existingMIME != mimeType) {
				return &campaignHandlerError{
					status: fasthttp.StatusConflict, message: "Stored campaign media digest conflicts with upload", operation: "verify existing campaign media",
				}
			}
		case errors.Is(existingErr, storage.ErrObjectNotFound), errors.Is(existingErr, os.ErrNotExist):
			var campaignReferences int64
			if referenceErr := scoped.DB.Unscoped().Model(&models.BulkMessageCampaign{}).
				Where("organization_id = ? AND header_media_local_path = ?", orgID, candidatePath).
				Count(&campaignReferences).Error; referenceErr != nil {
				return &campaignHandlerError{
					status: fasthttp.StatusInternalServerError, message: "Failed to verify campaign media references", operation: "verify campaign candidate references", cause: referenceErr,
				}
			}
			var messageReferences int64
			if referenceErr := scoped.DB.Unscoped().Model(&models.Message{}).
				Where("organization_id = ? AND media_url = ?", orgID, candidatePath).
				Count(&messageReferences).Error; referenceErr != nil {
				return &campaignHandlerError{
					status: fasthttp.StatusInternalServerError, message: "Failed to verify campaign media references", operation: "verify message candidate references", cause: referenceErr,
				}
			}
			storedPath, storeErr := scoped.saveCampaignMedia(ctx, orgID, campaignUUID.String(), data, mimeType)
			if storeErr != nil {
				// Put failures are transport-ambiguous. Without a create-only storage
				// primitive this invocation cannot prove ownership and must not delete.
				return &campaignHandlerError{
					status: fasthttp.StatusInternalServerError, message: "Failed to store campaign media", operation: "stage campaign media", cause: storeErr,
				}
			}
			if storedPath != candidatePath {
				return &campaignHandlerError{
					status: fasthttp.StatusInternalServerError, message: "Failed to store campaign media", operation: "verify staged campaign media path",
				}
			}
			cleanupCreatedCandidate = campaignReferences == 0 && messageReferences == 0
		default:
			return &campaignHandlerError{
				status: fasthttp.StatusInternalServerError, message: "Failed to verify campaign media storage", operation: "load candidate campaign media", cause: existingErr,
			}
		}

		// A completed retry of the same bytes, MIME type, and filename reuses the
		// committed provider ID only after the durable preview itself has been
		// verified or restored. This cannot resolve a process death after Meta
		// accepted an upload but before this transaction committed.
		if campaign.HeaderMediaID != "" &&
			campaign.HeaderMediaLocalPath == candidatePath &&
			campaign.HeaderMediaMimeType == mimeType &&
			campaign.HeaderMediaFilename == filename {
			mediaID = campaign.HeaderMediaID
			mediaPath = campaign.HeaderMediaLocalPath
			return nil
		}

		mediaID, accountErr = scoped.WhatsApp.UploadMedia(
			ctx,
			scoped.toWhatsAppAccount(lockedAccount),
			data,
			mimeType,
			fileHeader.Filename,
		)
		if accountErr != nil {
			// Meta transport and response-read failures are ambiguous. Preserve a
			// newly created durable candidate for later reconciliation/retry.
			cleanupCreatedCandidate = false
			return &campaignHandlerError{
				status:    fasthttp.StatusInternalServerError,
				message:   "Failed to upload media to WhatsApp",
				operation: "upload campaign media to WhatsApp",
				cause:     accountErr,
			}
		}

		updated := scoped.DB.WithContext(ctx).
			Model(&models.BulkMessageCampaign{}).
			Where(
				"id = ? AND organization_id = ? AND status = ? AND template_id = ? AND whats_app_account = ?",
				campaign.ID,
				orgID,
				models.CampaignStatusDraft,
				campaign.TemplateID,
				campaign.WhatsAppAccount,
			).
			Updates(map[string]any{
				"header_media_id":         mediaID,
				"header_media_filename":   filename,
				"header_media_mime_type":  mimeType,
				"header_media_local_path": mediaPath,
			})
		if updated.Error != nil {
			return &campaignHandlerError{
				status:    fasthttp.StatusInternalServerError,
				message:   "Failed to save media info",
				operation: "persist campaign media",
				cause:     updated.Error,
			}
		}
		if updated.RowsAffected != 1 {
			return &campaignHandlerError{
				status:    fasthttp.StatusConflict,
				message:   "Campaign changed while saving media",
				operation: "persist campaign media",
			}
		}
		return nil
	})
	if err != nil {
		// A raw transaction error occurs after the callback returned success and
		// includes an ambiguous COMMIT outcome. Cleanup is permitted only for a
		// definite callback failure after this invocation proved it created the
		// candidate. A fresh locked reference proof closes the retry race.
		var handlerErr *campaignHandlerError
		if cleanupCreatedCandidate && mediaPath != "" && errors.As(err, &handlerErr) {
			cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
			cleanupErr := a.deleteUnreferencedCampaignMedia(cleanupCtx, orgID, campaignUUID, mediaPath)
			cancelCleanup()
			if cleanupErr != nil {
				a.Log.Error("Failed to clean staged campaign media", "error", cleanupErr, "org_id", orgID, "media_path", mediaPath)
			}
		}

		if errors.As(err, &handlerErr) {
			if handlerErr.cause != nil {
				a.Log.Error("Campaign media upload failed", "operation", handlerErr.operation, "error", handlerErr.cause, "org_id", orgID, "campaign_id", campaignUUID)
			}
			return r.SendErrorEnvelope(handlerErr.status, handlerErr.message, nil, "")
		}
		a.Log.Error("Campaign media transaction failed", "error", err, "org_id", orgID, "campaign_id", campaignUUID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to save media info", nil, "")
	}

	a.Log.Info("Campaign media uploaded", "campaign_id", campaignUUID, "media_id", mediaID, "filename", fileHeader.Filename, "media_path", mediaPath)

	return r.SendEnvelope(map[string]any{
		"media_id":   mediaID,
		"filename":   fileHeader.Filename,
		"mime_type":  mimeType,
		"local_path": mediaPath,
		"message":    "Media uploaded successfully",
	})
}

// saveCampaignMedia stores uploaded media for preview.
func (a *App) saveCampaignMedia(ctx context.Context, orgID uuid.UUID, campaignID string, data []byte, mimeType string) (string, error) {
	filename := campaignMediaFilename(campaignID, data, mimeType)
	return a.saveTenantMedia(ctx, orgID, "campaigns", "campaigns", filename, data, mimeType)
}

func campaignMediaFilename(campaignID string, data []byte, mimeType string) string {
	ext := getExtensionFromMimeType(mimeType)
	if ext == "" {
		ext = ".bin"
	}
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%s-%x%s", campaignID, digest, ext)
}

func campaignTemplateAllowsMediaMIME(headerType, mimeType string) bool {
	headerType = strings.ToUpper(strings.TrimSpace(headerType))
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	switch headerType {
	case "IMAGE":
		return mimeType == "image/jpeg" || mimeType == "image/png" || mimeType == "image/webp"
	case "VIDEO":
		return mimeType == "video/mp4" || mimeType == "video/3gpp"
	case "DOCUMENT":
		return mimeType == "application/pdf" ||
			mimeType == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" ||
			mimeType == "application/vnd.openxmlformats-officedocument.wordprocessingml.document" ||
			mimeType == "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	default:
		return false
	}
}

// deleteUnreferencedCampaignMedia serializes cleanup with every upload for the
// campaign and deletes only an object that no committed campaign/message
// projection references. The caller must additionally prove it created the
// object and that the surrounding transaction did not have an ambiguous commit.
func (a *App) deleteUnreferencedCampaignMedia(
	ctx context.Context,
	orgID, campaignID uuid.UUID,
	mediaPath string,
) error {
	if a == nil || orgID == uuid.Nil || campaignID == uuid.Nil || strings.TrimSpace(mediaPath) == "" {
		return errors.New("campaign media cleanup identity is incomplete")
	}
	return a.WithCommittedTenantApp(orgID, func(scoped *App) error {
		var campaign models.BulkMessageCampaign
		loadErr := scoped.DB.WithContext(ctx).Unscoped().
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", campaignID, orgID).
			First(&campaign).Error
		if loadErr != nil && !errors.Is(loadErr, gorm.ErrRecordNotFound) {
			return loadErr
		}
		var campaignReferences int64
		if err := scoped.DB.WithContext(ctx).Unscoped().Model(&models.BulkMessageCampaign{}).
			Where("organization_id = ? AND header_media_local_path = ?", orgID, mediaPath).
			Count(&campaignReferences).Error; err != nil {
			return err
		}
		if campaignReferences != 0 {
			return errCampaignMediaCandidateReferenced
		}
		var messageReferences int64
		if err := scoped.DB.WithContext(ctx).Unscoped().Model(&models.Message{}).
			Where("organization_id = ? AND media_url = ?", orgID, mediaPath).
			Count(&messageReferences).Error; err != nil {
			return err
		}
		if messageReferences != 0 {
			return errCampaignMediaCandidateReferenced
		}
		return scoped.deleteTenantMedia(ctx, orgID, mediaPath)
	})
}

func (a *App) campaignMediaStoragePath(orgID uuid.UUID, campaignID string, data []byte, mimeType string) string {
	filename := campaignMediaFilename(campaignID, data, mimeType)
	if a.Config != nil && strings.EqualFold(a.Config.Storage.Type, "s3") {
		return tenantObjectKey(orgID, "campaigns", filename)
	}
	return path.Join("campaigns", filename)
}

// ServeCampaignMedia serves campaign media files for preview
func (a *App) ServeCampaignMedia(r *fastglue.Request) error {
	// Get auth context
	orgID, err := a.getOrgID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	// Get campaign ID from URL
	campaignUUID, err := parsePathUUID(r, "id", "campaign")
	if err != nil {
		return nil
	}

	// Find campaign and verify access
	campaign, err := findByIDAndOrg[models.BulkMessageCampaign](a.DB, r, campaignUUID, orgID, "Campaign")
	if err != nil {
		return nil
	}

	// Check if campaign has media
	if campaign.HeaderMediaLocalPath == "" {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "No media found", nil, "")
	}

	data, storedContentType, err := a.loadTenantMedia(r.RequestCtx, orgID, campaign.HeaderMediaLocalPath)
	if err != nil {
		if errors.Is(err, errInvalidStoredMediaKey) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid media path", nil, "")
		}
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, storage.ErrObjectNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "File not found", nil, "")
		}
		a.Log.Error("Failed to read campaign media", "key", campaign.HeaderMediaLocalPath, "error", err, "org_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to read file", nil, "")
	}

	contentType := storedContentType
	if contentType == "" {
		contentType = campaign.HeaderMediaMimeType
	}
	if contentType == "" {
		ext := strings.ToLower(filepath.Ext(campaign.HeaderMediaLocalPath))
		contentType = getMimeTypeFromExtension(ext)
	}

	r.RequestCtx.Response.Header.Set("Content-Type", contentType)
	r.RequestCtx.Response.Header.Set("Cache-Control", "private, max-age=3600")
	r.RequestCtx.SetBody(data)

	return nil
}

// incrementCampaignStat increments one tenant-bound campaign counter. Receipt
// processing calls it inside the same transaction as the exact recipient
// update, so a provider-neutral WAMID can never update a different campaign.
func (a *App) incrementCampaignStat(campaignID, organizationID uuid.UUID, status string) error {
	if campaignID == uuid.Nil || organizationID == uuid.Nil {
		return errors.New("campaign counter owner is incomplete")
	}

	var column string
	switch models.MessageStatus(status) {
	case models.MessageStatusDelivered:
		column = "delivered_count"
	case models.MessageStatusRead:
		column = "read_count"
	case models.MessageStatusFailed:
		column = "failed_count"
	default:
		// sent is already counted during processCampaign
		return nil
	}

	campaign := models.BulkMessageCampaign{BaseModel: models.BaseModel{ID: campaignID}, OrganizationID: organizationID}

	// atomic update and return updated record
	result := a.DB.Model(&campaign).
		Clauses(clause.Returning{}).
		Where("id = ? AND organization_id = ?", campaignID, organizationID).
		Update(column, gorm.Expr(column+" + 1"))

	if result.Error != nil {
		return fmt.Errorf("increment campaign stat: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return errors.New("campaign counter owner changed")
	}

	// Publish only after the tenant transaction commits. A status receipt can
	// update this counter inside a larger transaction; broadcasting here would
	// otherwise expose state that a later rollback removes.
	if a.WSHub != nil && result.RowsAffected > 0 {
		hub := a.WSHub
		organizationID := campaign.OrganizationID
		message := websocket.WSMessage{
			Type: websocket.TypeCampaignStatsUpdate,
			Payload: map[string]any{
				"campaign_id":     campaignID.String(),
				"status":          campaign.Status,
				"sent_count":      campaign.SentCount,
				"delivered_count": campaign.DeliveredCount,
				"read_count":      campaign.ReadCount,
				"failed_count":    campaign.FailedCount,
			},
		}
		a.afterTenantCommit(func() {
			hub.BroadcastToOrg(organizationID, message)
		})
	}
	return nil
}

// sanitizeFilename removes path separators, dangerous characters, and truncates length.
var safeFilenameRe = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

func sanitizeFilename(name string) string {
	// Strip any path component
	name = filepath.Base(name)
	// Replace unsafe characters
	name = safeFilenameRe.ReplaceAllString(name, "_")
	// Truncate to 255 chars
	if len(name) > 255 {
		name = name[:255]
	}
	if name == "" || name == "." || name == ".." {
		name = "unnamed"
	}
	return name
}
