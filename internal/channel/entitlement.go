package channel

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

const OmnichannelEntitlementKey = "omnichannel.enabled"

// HasDurableOmnichannelEntitlement evaluates the commercial entitlement without
// relying on an authenticated user. Callers must pass a tenant-scoped database
// handle when row-level security is enabled. Missing or non-permitting
// subscriptions fail closed, and an override cannot reopen an expired
// subscription.
func HasDurableOmnichannelEntitlement(
	db *gorm.DB,
	organizationID uuid.UUID,
	now time.Time,
) (bool, error) {
	if db == nil {
		return false, errors.New("database is required")
	}
	if organizationID == uuid.Nil {
		return false, errors.New("organization ID is required")
	}
	now = now.UTC()

	var subscription models.Subscription
	err := db.
		Where("organization_id = ?", organizationID).
		Order("created_at DESC").
		First(&subscription).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !subscriptionPermitsOmnichannel(&subscription, now) {
		return false, nil
	}

	value, exists := subscription.EntitlementsSnapshot[OmnichannelEntitlementKey]
	allowed := exists && durableEntitlementAllows(value)

	var override models.EntitlementOverride
	err = db.
		Where(
			"organization_id = ? AND key = ? AND is_active = ? AND starts_at <= ? AND (expires_at IS NULL OR expires_at > ?)",
			organizationID,
			OmnichannelEntitlementKey,
			true,
			now,
			now,
		).
		Order("starts_at DESC, created_at DESC").
		First(&override).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return allowed, nil
	}
	if err != nil {
		return false, err
	}
	return durableEntitlementAllows(entitlementOverrideValue(override.Value)), nil
}

func subscriptionPermitsOmnichannel(subscription *models.Subscription, now time.Time) bool {
	if subscription == nil {
		return false
	}
	switch subscription.Status {
	case models.SubscriptionStatusActive:
		return (subscription.CurrentPeriodEnd != nil && subscription.CurrentPeriodEnd.After(now)) ||
			(subscription.GraceUntil != nil && subscription.GraceUntil.After(now))
	case models.SubscriptionStatusTrialing:
		return subscription.TrialEndsAt != nil && subscription.TrialEndsAt.After(now)
	case models.SubscriptionStatusPastDue:
		return subscription.GraceUntil != nil && subscription.GraceUntil.After(now)
	default:
		return false
	}
}

func entitlementOverrideValue(value models.JSONB) any {
	if len(value) == 1 {
		if scalar, ok := value["value"]; ok {
			return scalar
		}
	}
	return value
}

func durableEntitlementAllows(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case int:
		return typed > 0
	case int32:
		return typed > 0
	case int64:
		return typed > 0
	case float32:
		return !math.IsNaN(float64(typed)) && typed > 0
	case float64:
		return !math.IsNaN(typed) && typed > 0
	case json.Number:
		number, err := typed.Float64()
		return err == nil && !math.IsNaN(number) && number > 0
	case string:
		normalized := strings.ToLower(strings.TrimSpace(typed))
		return normalized != "" &&
			normalized != "0" &&
			normalized != "false" &&
			normalized != "disabled" &&
			normalized != "none"
	case models.JSONB:
		if nested, ok := typed["value"]; ok {
			return durableEntitlementAllows(nested)
		}
		if enabled, ok := typed["enabled"]; ok {
			return durableEntitlementAllows(enabled)
		}
		return len(typed) > 0
	case map[string]any:
		return durableEntitlementAllows(models.JSONB(typed))
	default:
		return value != nil
	}
}
