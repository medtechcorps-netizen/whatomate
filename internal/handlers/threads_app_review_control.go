package handlers

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/threadsreview"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type approveThreadsAppReviewRequest struct {
	Evidence string `json:"evidence"`
}

type approveThreadsAppReviewResponse struct {
	OrganizationID uuid.UUID `json:"organization_id"`
	IntegrationID  uuid.UUID `json:"integration_id"`
	AppID          string    `json:"app_id"`
	Status         string    `json:"status"`
	ApprovedAt     time.Time `json:"approved_at"`
	ApprovedBy     uuid.UUID `json:"approved_by"`
	Enabled        bool      `json:"enabled"`
}

// ApproveOrganizationThreadsAppReview is the only production control plane
// that may mint approval evidence. Tenant integration writers cannot call it.
func (a *App) ApproveOrganizationThreadsAppReview(r *fastglue.Request) error {
	selectedOrgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.IsSuperAdmin(userID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Platform owner access required", nil, "")
	}
	targetOrgID, err := targetOrganizationID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid organization ID", nil, "")
	}
	if selectedOrgID != targetOrgID {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Selected organization does not match target organization", nil, "")
	}
	var request approveThreadsAppReviewRequest
	if err := decodeStrictThreadsSupportRequest(r, &request); err != nil {
		return nil
	}
	request.Evidence = strings.TrimSpace(request.Evidence)
	if request.Evidence == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "evidence is required", nil, "")
	}

	var response approveThreadsAppReviewResponse
	err = database.WithTenant(a.rootApp().DB, targetOrgID, func(tx *gorm.DB) error {
		var organization models.Organization
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", targetOrgID).
			First(&organization).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return &integrationClientError{status: fasthttp.StatusNotFound, message: "Organization not found"}
			}
			return err
		}
		var integration models.ProviderIntegration
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("organization_id = ? AND provider = ?", targetOrgID, integrationProviderThreads).
			First(&integration).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return &integrationClientError{status: fasthttp.StatusConflict, message: "Configure the Threads integration before recording approval"}
			}
			return err
		}
		appID := stringJSONValue(integration.Config, "app_id")
		redirectURI := stringJSONValue(integration.Config, "redirect_uri")
		rawAppID, appIDIsString := integration.Config["app_id"].(string)
		rawRedirectURI, redirectURIIsString := integration.Config["redirect_uri"].(string)
		if !appIDIsString || rawAppID != appID || !validThreadsOAuthID(appID) ||
			!redirectURIIsString || rawRedirectURI != redirectURI || redirectURI == "" ||
			validateThreadsRedirectURI(redirectURI) != nil ||
			!encryptedCredentialFlag(&integration, "app_secret").Configured ||
			!encryptedCredentialFlag(&integration, "webhook_verify_token").Configured {
			return &integrationClientError{
				status:  fasthttp.StatusConflict,
				message: "Configure the Threads App ID, exact OAuth callback, App Secret, and webhook verify token before recording approval",
			}
		}
		now := time.Now().UTC()
		approvedConfig, ok := threadsreview.RecordApproval(integration.Config, appID, request.Evidence, userID, now)
		if !ok {
			return &integrationClientError{status: fasthttp.StatusBadRequest, message: "evidence is invalid or too long"}
		}
		oldSnapshot := integrationAuditSnapshot(integrationProviderThreads, &organization, &integration, nil)
		integration.Config = approvedConfig
		// Approval never activates a stale legacy connection. A tenant admin must
		// explicitly enable and authorize after the evidence is recorded.
		integration.Enabled = false
		integration.LastTestedAt = nil
		integration.LastSuccessfulAt = nil
		integration.LastErrorCode = ""
		integration.LastErrorMessage = ""
		integration.ValidationToken = ""
		integration.UpdatedByID = &userID
		if err := disconnectThreadsChannelAccounts(tx, targetOrgID, userID, now); err != nil {
			return err
		}
		if err := tx.Save(&integration).Error; err != nil {
			return err
		}
		newSnapshot := integrationAuditSnapshot(integrationProviderThreads, &organization, &integration, nil)
		newSnapshot["approval_evidence_recorded"] = true
		if err := audit.LogAudit(
			tx,
			targetOrgID,
			userID,
			audit.GetUserName(tx, userID),
			models.ResourceSettingsIntegrations,
			integration.ID,
			models.AuditActionUpdated,
			oldSnapshot,
			newSnapshot,
		); err != nil {
			return err
		}
		response = approveThreadsAppReviewResponse{
			OrganizationID: targetOrgID,
			IntegrationID:  integration.ID,
			AppID:          appID,
			Status:         threadsAppReviewApprovedStatus,
			ApprovedAt:     now,
			ApprovedBy:     userID,
			Enabled:        false,
		}
		return nil
	})
	if err != nil {
		return a.sendIntegrationMutationError(r, err)
	}
	return r.SendEnvelope(response)
}
