package handlers

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RevokeThreadsPublicEngagementSupportRequest combines an immutable target
// identity with the operator reason. The override key, source, and desired
// state are fixed by the endpoint and cannot be supplied by callers.
type RevokeThreadsPublicEngagementSupportRequest struct {
	OverrideID uuid.UUID `json:"override_id"`
	Reason     string    `json:"reason"`
}

// ThreadsPublicEngagementSupportStatusResponse exposes only the immutable ID
// needed to target a later revoke request. It never exposes either support
// reason. Active is true only for one currently effective, revocable support
// grant; conflicting active rows fail closed instead of being represented.
type ThreadsPublicEngagementSupportStatusResponse struct {
	OrganizationID uuid.UUID  `json:"organization_id"`
	EntitlementKey string     `json:"entitlement_key"`
	Active         bool       `json:"active"`
	OverrideID     *uuid.UUID `json:"override_id,omitempty"`
	Source         string     `json:"source,omitempty"`
	StartsAt       *time.Time `json:"starts_at,omitempty"`
}

// RevokeThreadsPublicEngagementSupportResponse excludes both the original
// support reason and the revocation reason. Revoked reports whether this call
// performed the state transition; it is false for a safe idempotent retry.
type RevokeThreadsPublicEngagementSupportResponse struct {
	OrganizationID   uuid.UUID `json:"organization_id"`
	EntitlementKey   string    `json:"entitlement_key"`
	OverrideID       uuid.UUID `json:"override_id"`
	Source           string    `json:"source"`
	RevokedAt        time.Time `json:"revoked_at"`
	Revoked          bool      `json:"revoked"`
	EffectiveEnabled bool      `json:"effective_enabled"`
}

type threadsSupportRevocationAuditSnapshot struct {
	EntitlementKey   string     `json:"entitlement_key"`
	Enabled          bool       `json:"enabled"`
	Source           string     `json:"source"`
	SupportReason    string     `json:"support_reason"`
	State            string     `json:"state"`
	IsActive         bool       `json:"is_active"`
	StartsAt         time.Time  `json:"starts_at"`
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
	RevokedByID      *uuid.UUID `json:"revoked_by_id,omitempty"`
	RevocationReason string     `json:"revocation_reason,omitempty"`
}

// GetOrganizationThreadsPublicEngagementSupportStatus returns the immutable
// identity of the one support grant that the revoke endpoint may target.
//
// GET /api/admin/organizations/{target_organization_id}/entitlements/threads-public-engagement/support-status
func (a *App) GetOrganizationThreadsPublicEngagementSupportStatus(r *fastglue.Request) error {
	selectedOrgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.IsSuperAdmin(userID) {
		return r.SendErrorEnvelope(
			fasthttp.StatusForbidden,
			"Platform owner access required",
			nil,
			"",
		)
	}

	targetOrgID, err := targetOrganizationID(r)
	if err != nil {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"Invalid organization ID",
			nil,
			"",
		)
	}
	if selectedOrgID != targetOrgID {
		return r.SendErrorEnvelope(
			fasthttp.StatusConflict,
			"Selected organization does not match target organization",
			nil,
			"",
		)
	}

	response := ThreadsPublicEngagementSupportStatusResponse{
		OrganizationID: targetOrgID,
		EntitlementKey: channelapi.ThreadsPublicEngagementEntitlementKey,
	}
	err = database.WithTenant(a.rootApp().DB, targetOrgID, func(tx *gorm.DB) error {
		var organization models.Organization
		if err := tx.Where("id = ?", targetOrgID).First(&organization).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return newProductCommercialClientError(
					fasthttp.StatusNotFound,
					"Organization not found",
				)
			}
			return err
		}

		var activeOverrides []models.EntitlementOverride
		if err := tx.Where(
			"organization_id = ? AND key = ? AND is_active = ?",
			targetOrgID,
			channelapi.ThreadsPublicEngagementEntitlementKey,
			true,
		).
			Order("starts_at DESC, created_at DESC, id DESC").
			Find(&activeOverrides).Error; err != nil {
			return err
		}
		if len(activeOverrides) == 0 {
			return nil
		}
		if len(activeOverrides) != 1 ||
			!threadsSupportOverrideIsRevocable(&activeOverrides[0], time.Now().UTC()) {
			return newProductCommercialClientError(
				fasthttp.StatusConflict,
				"The active Threads public engagement override is not a revocable support grant",
			)
		}

		override := &activeOverrides[0]
		startsAt := override.StartsAt
		response.Active = true
		response.OverrideID = &override.ID
		response.Source = threadsSupportOverrideSource
		response.StartsAt = &startsAt
		return nil
	})
	if err != nil {
		return a.sendProductCommercialError(
			r,
			"read Threads public engagement support status",
			err,
		)
	}
	return r.SendEnvelope(response)
}

// RevokeOrganizationThreadsPublicEngagementSupport revokes exactly one
// currently effective support-source override for the selected organization.
// It cannot revoke plan entitlements, overrides from another source, or a
// scheduled override.
//
// POST /api/admin/organizations/{target_organization_id}/entitlements/threads-public-engagement/revoke-support
func (a *App) RevokeOrganizationThreadsPublicEngagementSupport(r *fastglue.Request) error {
	selectedOrgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.IsSuperAdmin(userID) {
		return r.SendErrorEnvelope(
			fasthttp.StatusForbidden,
			"Platform owner access required",
			nil,
			"",
		)
	}

	targetOrgID, err := targetOrganizationID(r)
	if err != nil {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"Invalid organization ID",
			nil,
			"",
		)
	}
	if selectedOrgID != targetOrgID {
		return r.SendErrorEnvelope(
			fasthttp.StatusConflict,
			"Selected organization does not match target organization",
			nil,
			"",
		)
	}

	var req RevokeThreadsPublicEngagementSupportRequest
	if err := decodeStrictThreadsSupportRequest(r, &req); err != nil {
		return nil
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"reason is required",
			nil,
			"",
		)
	}
	if utf8.RuneCountInString(req.Reason) > threadsSupportMaxReasonRunes {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"reason is too long",
			nil,
			"",
		)
	}
	if req.OverrideID == uuid.Nil {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"override_id is required",
			nil,
			"",
		)
	}

	var response RevokeThreadsPublicEngagementSupportResponse
	err = database.WithTenant(a.rootApp().DB, targetOrgID, func(tx *gorm.DB) error {
		now := time.Now().UTC()

		// Serializing on the organization row makes the active-set check and
		// transition atomic with the matching support grant endpoint.
		var organization models.Organization
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", targetOrgID).
			First(&organization).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return newProductCommercialClientError(
					fasthttp.StatusNotFound,
					"Organization not found",
				)
			}
			return err
		}

		var activeOverrides []models.EntitlementOverride
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"organization_id = ? AND key = ? AND is_active = ?",
				targetOrgID,
				channelapi.ThreadsPublicEngagementEntitlementKey,
				true,
			).
			Order("starts_at DESC, created_at DESC, id DESC").
			Find(&activeOverrides).Error; err != nil {
			return err
		}

		if len(activeOverrides) == 0 {
			if err := threadsSupportResolveRevocationRetry(
				tx,
				targetOrgID,
				req.OverrideID,
				req.Reason,
				&response,
			); err != nil {
				return err
			}
			return a.verifyThreadsSupportRevokedInTransaction(
				tx,
				userID,
				targetOrgID,
				&response,
			)
		}
		if len(activeOverrides) != 1 ||
			!threadsSupportOverrideIsRevocable(&activeOverrides[0], now) {
			return newProductCommercialClientError(
				fasthttp.StatusConflict,
				"The active Threads public engagement override is not a revocable support grant",
			)
		}
		if activeOverrides[0].ID != req.OverrideID {
			return newProductCommercialClientError(
				fasthttp.StatusConflict,
				"Active support override changed; refresh before revoking",
			)
		}

		override := &activeOverrides[0]
		oldSnapshot := threadsSupportRevocationSnapshot(override)
		revokedByID := userID
		updates := map[string]any{
			"is_active":         false,
			"revoked_by_id":     revokedByID,
			"revoked_at":        now,
			"revocation_reason": req.Reason,
		}
		result := tx.Model(&models.EntitlementOverride{}).
			Where(
				"id = ? AND organization_id = ? AND is_active = ?",
				override.ID,
				targetOrgID,
				true,
			).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("support override changed while it was being revoked")
		}

		override.IsActive = false
		override.RevokedByID = &revokedByID
		override.RevokedAt = &now
		override.RevocationReason = req.Reason
		response = threadsSupportRevocationResponse(
			targetOrgID,
			override,
			true,
		)

		if err := audit.LogAudit(
			tx,
			targetOrgID,
			userID,
			audit.GetUserName(tx, userID),
			threadsSupportAuditResource,
			override.ID,
			models.AuditActionUpdated,
			oldSnapshot,
			threadsSupportRevocationSnapshot(override),
		); err != nil {
			return err
		}
		return a.verifyThreadsSupportRevokedInTransaction(
			tx,
			userID,
			targetOrgID,
			&response,
		)
	})
	if err != nil {
		return a.sendProductCommercialError(
			r,
			"revoke Threads public engagement support entitlement",
			err,
		)
	}

	return r.SendEnvelope(response)
}

func (a *App) verifyThreadsSupportRevokedInTransaction(
	tx *gorm.DB,
	userID, organizationID uuid.UUID,
	response *RevokeThreadsPublicEngagementSupportResponse,
) error {
	decision, err := a.rootApp().scopedApp(tx, organizationID).EvaluateProductEntitlement(
		userID,
		organizationID,
		channelapi.ThreadsPublicEngagementEntitlementKey,
	)
	if err != nil {
		return err
	}
	if decision.Overridden {
		return errors.New("threads public engagement support override remains effective")
	}
	response.EffectiveEnabled = decision.Allowed
	return nil
}

func threadsSupportResolveRevocationRetry(
	tx *gorm.DB,
	organizationID uuid.UUID,
	overrideID uuid.UUID,
	revocationReason string,
	response *RevokeThreadsPublicEngagementSupportResponse,
) error {
	var override models.EntitlementOverride
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where(
			"id = ? AND organization_id = ? AND key = ? AND source = ? AND is_active = ?",
			overrideID,
			organizationID,
			channelapi.ThreadsPublicEngagementEntitlementKey,
			threadsSupportOverrideSource,
			false,
		).
		First(&override).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return newProductCommercialClientError(
				fasthttp.StatusNotFound,
				"Support Threads public engagement override not found",
			)
		}
		return err
	}
	if !threadsSupportOverrideWasSupportGrant(&override) ||
		strings.TrimSpace(override.RevocationReason) != revocationReason {
		return newProductCommercialClientError(
			fasthttp.StatusConflict,
			"Revocation retry does not match the recorded operation",
		)
	}

	*response = threadsSupportRevocationResponse(
		organizationID,
		&override,
		false,
	)
	return nil
}

func threadsSupportOverrideWasSupportGrant(override *models.EntitlementOverride) bool {
	return override != nil &&
		override.Source == threadsSupportOverrideSource &&
		override.ValueType == models.EntitlementValueTypeBoolean &&
		productCommercialEntitlementAllows(override.Value) &&
		override.RevokedAt != nil &&
		override.RevokedByID != nil &&
		strings.TrimSpace(override.RevocationReason) != ""
}

func threadsSupportOverrideIsRevocable(
	override *models.EntitlementOverride,
	now time.Time,
) bool {
	return threadsSupportOverrideWasSupportGrantBeforeRevocation(override) &&
		threadsSupportOverrideIsEffectiveAt(override, now)
}

func threadsSupportOverrideWasSupportGrantBeforeRevocation(
	override *models.EntitlementOverride,
) bool {
	return override != nil &&
		override.Source == threadsSupportOverrideSource &&
		override.ValueType == models.EntitlementValueTypeBoolean &&
		productCommercialEntitlementAllows(override.Value) &&
		override.RevokedAt == nil &&
		override.RevokedByID == nil &&
		strings.TrimSpace(override.RevocationReason) == ""
}

func threadsSupportRevocationSnapshot(
	override *models.EntitlementOverride,
) threadsSupportRevocationAuditSnapshot {
	state := "active"
	if !override.IsActive {
		state = "revoked"
	}
	return threadsSupportRevocationAuditSnapshot{
		EntitlementKey:   override.Key,
		Enabled:          productCommercialEntitlementAllows(override.Value),
		Source:           override.Source,
		SupportReason:    override.Reason,
		State:            state,
		IsActive:         override.IsActive,
		StartsAt:         override.StartsAt,
		RevokedAt:        override.RevokedAt,
		RevokedByID:      override.RevokedByID,
		RevocationReason: override.RevocationReason,
	}
}

func threadsSupportRevocationResponse(
	organizationID uuid.UUID,
	override *models.EntitlementOverride,
	revoked bool,
) RevokeThreadsPublicEngagementSupportResponse {
	response := RevokeThreadsPublicEngagementSupportResponse{
		OrganizationID: organizationID,
		EntitlementKey: channelapi.ThreadsPublicEngagementEntitlementKey,
		OverrideID:     override.ID,
		Source:         override.Source,
		Revoked:        revoked,
	}
	if override.RevokedAt != nil {
		response.RevokedAt = *override.RevokedAt
	}
	return response
}
