package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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

const (
	threadsSupportGrowthPlanCode = "rereply-growth"
	threadsSupportOverrideSource = "support"
	threadsSupportAuditResource  = "entitlement_override"
	threadsSupportMaxReasonRunes = 2000
)

var threadsSupportEligibleSubscriptionStatuses = []models.SubscriptionStatus{
	models.SubscriptionStatusTrialing,
	models.SubscriptionStatusActive,
}

// EnableThreadsPublicEngagementRequest deliberately has no entitlement key or
// desired value. The support surface can only enable the single reviewed
// Threads public-engagement capability.
type EnableThreadsPublicEngagementRequest struct {
	Reason string `json:"reason"`
}

// EnableThreadsPublicEngagementResponse is safe for the platform control
// plane: it excludes subscription provider data and the support reason.
type EnableThreadsPublicEngagementResponse struct {
	OrganizationID     uuid.UUID                 `json:"organization_id"`
	EntitlementKey     string                    `json:"entitlement_key"`
	OverrideID         uuid.UUID                 `json:"override_id"`
	Source             string                    `json:"source"`
	StartsAt           time.Time                 `json:"starts_at"`
	Created            bool                      `json:"created"`
	EffectiveEnabled   bool                      `json:"effective_enabled"`
	PlanCode           string                    `json:"plan_code"`
	SubscriptionStatus models.SubscriptionStatus `json:"subscription_status"`
}

type threadsSupportAuditSnapshot struct {
	EntitlementKey string    `json:"entitlement_key"`
	Enabled        bool      `json:"enabled"`
	Source         string    `json:"source"`
	Reason         string    `json:"reason"`
	StartsAt       time.Time `json:"starts_at"`
}

// EnableOrganizationThreadsPublicEngagement grants the single reviewed
// Threads public-engagement entitlement to one explicitly addressed
// organization. It cannot disable an entitlement or mutate arbitrary keys.
//
// Recommended route:
// POST /api/admin/organizations/{organization_id}/entitlements/threads-public-engagement/enable
func (a *App) EnableOrganizationThreadsPublicEngagement(r *fastglue.Request) error {
	_, userID, err := a.getOrgAndUserID(r)
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

	targetOrgID, err := productCommercialTargetOrganizationID(r)
	if err != nil {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"Invalid organization ID",
			nil,
			"",
		)
	}

	var req EnableThreadsPublicEngagementRequest
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

	var response EnableThreadsPublicEngagementResponse
	err = database.WithTenant(a.rootApp().DB, targetOrgID, func(tx *gorm.DB) error {
		now := time.Now().UTC()

		// The organization row serializes retries for an organization even when
		// no override exists yet, preventing duplicate active rows.
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

		var subscription models.Subscription
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"organization_id = ? AND status IN ?",
				targetOrgID,
				threadsSupportEligibleSubscriptionStatuses,
			).
			Order("created_at DESC, id DESC").
			First(&subscription).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return newProductCommercialClientError(
					fasthttp.StatusConflict,
					"A current ReReply Growth subscription is required",
				)
			}
			return err
		}

		var plan models.Plan
		if err := tx.Select("id", "code").
			Where("id = ?", subscription.PlanID).
			First(&plan).Error; err != nil {
			return err
		}
		if plan.Code != threadsSupportGrowthPlanCode ||
			!threadsSupportSubscriptionIsUnexpired(&subscription, now) {
			return newProductCommercialClientError(
				fasthttp.StatusConflict,
				"An active, unexpired ReReply Growth subscription is required",
			)
		}

		omnichannelValue := subscription.EntitlementsSnapshot[channelapi.OmnichannelEntitlementKey]
		omnichannelOverride, err := productCommercialActiveOverride(
			tx,
			targetOrgID,
			channelapi.OmnichannelEntitlementKey,
			now,
		)
		if err != nil {
			return err
		}
		if omnichannelOverride != nil {
			omnichannelValue = productCommercialEntitlementValue(
				omnichannelOverride.Value,
			)
		}
		if !productCommercialEntitlementAllows(omnichannelValue) {
			return newProductCommercialClientError(
				fasthttp.StatusConflict,
				"Effective omnichannel entitlement must be enabled first",
			)
		}

		var activeOverrides []models.EntitlementOverride
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"organization_id = ? AND key = ? AND is_active = ?",
				targetOrgID,
				channelapi.ThreadsPublicEngagementEntitlementKey,
				true,
			).
			Order("starts_at DESC, created_at DESC").
			Find(&activeOverrides).Error; err != nil {
			return err
		}
		if len(activeOverrides) > 0 {
			if len(activeOverrides) == 1 &&
				threadsSupportOverrideIsEffectiveAt(&activeOverrides[0], now) &&
				threadsSupportOverrideMatchesRequest(&activeOverrides[0], req.Reason) {
				response = threadsSupportResponse(
					targetOrgID,
					&activeOverrides[0],
					&subscription,
					false,
				)
				return nil
			}
			return newProductCommercialClientError(
				fasthttp.StatusConflict,
				"An active Threads public engagement override already exists",
			)
		}

		override := models.EntitlementOverride{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: targetOrgID,
			Key:            channelapi.ThreadsPublicEngagementEntitlementKey,
			ValueType:      models.EntitlementValueTypeBoolean,
			Value:          models.JSONB{"value": true},
			Source:         threadsSupportOverrideSource,
			Reason:         req.Reason,
			IsActive:       true,
			StartsAt:       now,
			CreatedByID:    userID,
		}
		if err := tx.Create(&override).Error; err != nil {
			return err
		}

		response = threadsSupportResponse(
			targetOrgID,
			&override,
			&subscription,
			true,
		)
		if !response.EffectiveEnabled {
			return errors.New("threads public engagement entitlement did not become effective")
		}

		return audit.LogAudit(
			tx,
			targetOrgID,
			userID,
			audit.GetUserName(tx, userID),
			threadsSupportAuditResource,
			override.ID,
			models.AuditActionCreated,
			nil,
			threadsSupportAuditSnapshot{
				EntitlementKey: override.Key,
				Enabled:        true,
				Source:         override.Source,
				Reason:         override.Reason,
				StartsAt:       override.StartsAt,
			},
		)
	})
	if err != nil {
		return a.sendProductCommercialError(
			r,
			"enable Threads public engagement entitlement",
			err,
		)
	}

	// Resolve again through the product authorization path after commit. This
	// confirms the durable decision seen by feature middleware, not merely the
	// value of the row written in this transaction.
	err = a.WithTenantApp(targetOrgID, func(scoped *App) error {
		decision, resolveErr := scoped.EvaluateProductEntitlement(
			userID,
			targetOrgID,
			channelapi.ThreadsPublicEngagementEntitlementKey,
		)
		if resolveErr != nil {
			return resolveErr
		}
		if !decision.Allowed || !decision.Overridden {
			return errors.New(
				"threads public engagement entitlement did not become effective",
			)
		}
		response.EffectiveEnabled = true
		return nil
	})
	if err != nil {
		return a.sendProductCommercialError(
			r,
			"verify Threads public engagement entitlement",
			err,
		)
	}
	return r.SendEnvelope(response)
}

func threadsSupportSubscriptionIsUnexpired(
	subscription *models.Subscription,
	now time.Time,
) bool {
	if subscription == nil {
		return false
	}
	switch subscription.Status {
	case models.SubscriptionStatusTrialing:
		return subscription.TrialEndsAt != nil && subscription.TrialEndsAt.After(now)
	case models.SubscriptionStatusActive:
		return subscription.CurrentPeriodEnd != nil &&
			subscription.CurrentPeriodEnd.After(now)
	default:
		return false
	}
}

func decodeStrictThreadsSupportRequest(
	r *fastglue.Request,
	req *EnableThreadsPublicEngagementRequest,
) error {
	decoder := json.NewDecoder(bytes.NewReader(r.RequestCtx.PostBody()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(req); err != nil {
		_ = r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
		return errEnvelopeSent
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		_ = r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
		return errEnvelopeSent
	}
	return nil
}

func threadsSupportOverrideIsEffectiveAt(
	override *models.EntitlementOverride,
	now time.Time,
) bool {
	return override != nil &&
		!override.StartsAt.After(now) &&
		(override.ExpiresAt == nil || override.ExpiresAt.After(now))
}

func threadsSupportOverrideMatchesRequest(
	override *models.EntitlementOverride,
	reason string,
) bool {
	return override != nil &&
		override.Source == threadsSupportOverrideSource &&
		override.ValueType == models.EntitlementValueTypeBoolean &&
		strings.TrimSpace(override.Reason) == reason &&
		productCommercialEntitlementAllows(override.Value)
}

func threadsSupportResponse(
	organizationID uuid.UUID,
	override *models.EntitlementOverride,
	subscription *models.Subscription,
	created bool,
) EnableThreadsPublicEngagementResponse {
	return EnableThreadsPublicEngagementResponse{
		OrganizationID: organizationID,
		EntitlementKey: channelapi.ThreadsPublicEngagementEntitlementKey,
		OverrideID:     override.ID,
		Source:         override.Source,
		StartsAt:       override.StartsAt,
		Created:        created,
		EffectiveEnabled: threadsSupportSubscriptionIsUnexpired(
			subscription,
			time.Now().UTC(),
		) && productCommercialEntitlementAllows(override.Value),
		PlanCode:           threadsSupportGrowthPlanCode,
		SubscriptionStatus: subscription.Status,
	}
}
