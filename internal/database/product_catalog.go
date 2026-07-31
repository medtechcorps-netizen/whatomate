package database

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

const (
	reReplyCatalogScopeKey          = "platform"
	reReplyCatalogSeedVersion       = 1
	reReplyCatalogSeedVersionKey    = "rereply_catalog_seed_version"
	reReplyLegacyGrowthMonthlyPrice = "rereply-growth-myr-month"

	reReplyStarterPlanCode  = "rereply-starter"
	reReplyStarterPriceCode = "rereply-starter-myr-month-2026-v1"
	reReplyGrowthPlanCode   = "rereply-growth"
	reReplyGrowthPriceCode  = "rereply-growth-myr-month-2026-v1"
)

type reReplyCatalogPlanSeed struct {
	Code             string
	Name             string
	Description      string
	DisplayOrder     int
	TrialDays        int
	PriceCode        string
	UnitAmountMinor  int64
	EntitlementState map[string]bool
}

var reReplyProductCatalog = []reReplyCatalogPlanSeed{
	{
		Code:            reReplyStarterPlanCode,
		Name:            "ReReply Starter",
		Description:     "An always-on WhatsApp front desk with chatbot, campaigns, templates and customer conversations.",
		DisplayOrder:    10,
		TrialDays:       14,
		PriceCode:       reReplyStarterPriceCode,
		UnitAmountMinor: 30000,
		EntitlementState: map[string]bool{
			"omnichannel.enabled":               false,
			"crm.enabled":                       false,
			"bookings.enabled":                  false,
			"commerce.enabled":                  false,
			"copilot.enabled":                   false,
			"threads.public_engagement.enabled": false,
		},
	},
	{
		Code:            reReplyGrowthPlanCode,
		Name:            "ReReply Growth",
		Description:     "One customer journey across shared inbox, CRM, automations, bookings, commerce and reviewed AI.",
		DisplayOrder:    20,
		TrialDays:       14,
		PriceCode:       reReplyGrowthPriceCode,
		UnitAmountMinor: 60000,
		EntitlementState: map[string]bool{
			"omnichannel.enabled":               true,
			"crm.enabled":                       true,
			"bookings.enabled":                  true,
			"commerce.enabled":                  true,
			"copilot.enabled":                   true,
			"threads.public_engagement.enabled": false,
		},
	},
}

// EnsureReReplyProductCatalog publishes the approved, assignable ReReply
// workspace plans. Calling (Sprint) and Enterprise remain presentation-only
// until their access can be enforced safely by the commercial entitlement
// layer.
//
// Published prices are immutable. Every approved selling price receives a new
// versioned code; this seed marks zero-value monthly placeholders as retired
// from new assignments without deactivating or rewriting their identities.
// Existing subscriptions retain their price identity and entitlement snapshot.
func EnsureReReplyProductCatalog(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		// Application replicas can start together during a deployment. Serialize
		// this small catalog data migration so two processes cannot both observe
		// a missing row and race on its unique key.
		if err := tx.Exec(
			"SELECT pg_advisory_xact_lock(hashtext(?))",
			"rereply-product-catalog-v1",
		).Error; err != nil {
			return fmt.Errorf("lock ReReply product catalog migration: %w", err)
		}
		for _, seed := range reReplyProductCatalog {
			if err := ensureReReplyCatalogPlan(tx, seed); err != nil {
				return fmt.Errorf("ensure %s product plan: %w", seed.Code, err)
			}
		}
		return nil
	})
}

func ensureReReplyCatalogPlan(tx *gorm.DB, seed reReplyCatalogPlanSeed) error {
	var plan models.Plan
	err := tx.Unscoped().Where(
		"scope_key = ? AND code = ?",
		reReplyCatalogScopeKey,
		seed.Code,
	).First(&plan).Error

	now := time.Now().UTC()
	shouldBackfill := false
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		plan = models.Plan{
			BaseModel:    models.BaseModel{ID: uuid.New()},
			ScopeKey:     reReplyCatalogScopeKey,
			Code:         seed.Code,
			Name:         seed.Name,
			Description:  seed.Description,
			Status:       models.CommercialPlanStatusActive,
			Vertical:     "general",
			TrialDays:    seed.TrialDays,
			DisplayOrder: seed.DisplayOrder,
			IsPublic:     true,
			Metadata: models.JSONB{
				reReplyCatalogSeedVersionKey: reReplyCatalogSeedVersion,
			},
			PublishedAt: &now,
		}
		if err := tx.Create(&plan).Error; err != nil {
			return err
		}
		shouldBackfill = true
	case err != nil:
		return err
	case plan.DeletedAt.Valid:
		// A soft delete is an explicit operator action. Do not resurrect the
		// plan during application startup.
		return nil
	default:
		if reReplyCatalogPlanSeedVersion(plan.Metadata) >= reReplyCatalogSeedVersion {
			break
		}
		metadata := cloneReReplyCatalogMetadata(plan.Metadata)
		metadata[reReplyCatalogSeedVersionKey] = reReplyCatalogSeedVersion
		updates := map[string]any{
			"name":          seed.Name,
			"description":   seed.Description,
			"status":        models.CommercialPlanStatusActive,
			"vertical":      "general",
			"trial_days":    seed.TrialDays,
			"display_order": seed.DisplayOrder,
			"is_public":     true,
			"archived_at":   nil,
			"metadata":      metadata,
		}
		if plan.PublishedAt == nil {
			updates["published_at"] = now
		}
		if err := tx.Model(&models.Plan{}).
			Where("id = ?", plan.ID).
			Updates(updates).Error; err != nil {
			return err
		}
		shouldBackfill = true
	}

	if err := ensureReReplyCatalogPrice(tx, plan.ID, seed); err != nil {
		return err
	}
	if !shouldBackfill {
		return nil
	}
	if err := retireReReplyLegacyGrowthPrice(tx, plan.ID, seed); err != nil {
		return err
	}
	return ensureReReplyCatalogEntitlements(tx, plan.ID, seed.EntitlementState)
}

func ensureReReplyCatalogPrice(
	tx *gorm.DB,
	planID uuid.UUID,
	seed reReplyCatalogPlanSeed,
) error {
	var price models.PlanPrice
	err := tx.Unscoped().Where(
		"plan_id = ? AND code = ?",
		planID,
		seed.PriceCode,
	).First(&price).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		price = models.PlanPrice{
			BaseModel:       models.BaseModel{ID: uuid.New()},
			PlanID:          planID,
			Code:            seed.PriceCode,
			Provider:        models.BillingProviderManual,
			Currency:        "MYR",
			UnitAmountMinor: seed.UnitAmountMinor,
			Interval:        models.BillingIntervalMonth,
			IntervalCount:   1,
			TaxBehavior:     "exclusive",
			IsActive:        true,
			ProviderData:    models.JSONB{},
			Metadata:        models.JSONB{},
		}
		if err := tx.Create(&price).Error; err != nil {
			return err
		}
	case err != nil:
		return err
	case price.DeletedAt.Valid:
		// Respect explicit retirement. The public catalog will omit the deleted
		// price and operators can publish a new versioned price when appropriate.
		return nil
	default:
		if price.Provider != models.BillingProviderManual ||
			price.Currency != "MYR" ||
			price.UnitAmountMinor != seed.UnitAmountMinor ||
			price.SetupAmountMinor != 0 ||
			price.Interval != models.BillingIntervalMonth ||
			price.IntervalCount != 1 ||
			price.TaxBehavior != "exclusive" ||
			price.EffectiveFrom != nil ||
			price.EffectiveUntil != nil {
			return fmt.Errorf(
				"published price %s does not match the approved immutable price",
				seed.PriceCode,
			)
		}
	}
	return nil
}

func retireReReplyLegacyGrowthPrice(
	tx *gorm.DB,
	planID uuid.UUID,
	seed reReplyCatalogPlanSeed,
) error {
	if seed.Code != reReplyGrowthPlanCode {
		return nil
	}

	// The original production Growth catalog used this exact zero-value code.
	// Keep the row active so existing subscriptions retain resolvable immutable
	// identities, but reject it for all new assignments. Do not match arbitrary
	// RM0 prices: an operator may intentionally publish a grant or promotion.
	var placeholder models.PlanPrice
	err := tx.Where(
		"plan_id = ? AND code = ? AND provider = ? AND currency = ? AND interval = ? AND interval_count = ? AND unit_amount_minor = 0",
		planID,
		reReplyLegacyGrowthMonthlyPrice,
		models.BillingProviderManual,
		"MYR",
		models.BillingIntervalMonth,
		1,
	).First(&placeholder).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	metadata := cloneReReplyCatalogMetadata(placeholder.Metadata)
	if assignable, configured := metadata["assignable"].(bool); configured &&
		!assignable &&
		metadata["replacement_price_code"] == seed.PriceCode {
		return nil
	}
	metadata["assignable"] = false
	metadata["replacement_price_code"] = seed.PriceCode
	return tx.Model(&models.PlanPrice{}).
		Where("id = ?", placeholder.ID).
		Update("metadata", metadata).Error
}

func reReplyCatalogPlanSeedVersion(metadata models.JSONB) int {
	switch value := metadata[reReplyCatalogSeedVersionKey].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func cloneReReplyCatalogMetadata(metadata models.JSONB) models.JSONB {
	cloned := make(models.JSONB, len(metadata)+1)
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func ensureReReplyCatalogEntitlements(
	tx *gorm.DB,
	planID uuid.UUID,
	entitlements map[string]bool,
) error {
	for key, enabled := range entitlements {
		var entitlement models.PlanEntitlement
		err := tx.Unscoped().Where("plan_id = ? AND key = ?", planID, key).
			First(&entitlement).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			entitlement = models.PlanEntitlement{
				BaseModel:   models.BaseModel{ID: uuid.New()},
				PlanID:      planID,
				Key:         key,
				ValueType:   models.EntitlementValueTypeBoolean,
				Value:       models.JSONB{"value": enabled},
				Enforcement: models.EntitlementEnforcementHard,
				Description: reReplyEntitlementDescription(key),
			}
			if err := tx.Create(&entitlement).Error; err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if entitlement.DeletedAt.Valid {
			return fmt.Errorf(
				"required entitlement %s was explicitly deleted from plan %s",
				key,
				planID,
			)
		}
		if err := tx.Model(&models.PlanEntitlement{}).
			Where("id = ?", entitlement.ID).
			Updates(map[string]any{
				"value_type":  models.EntitlementValueTypeBoolean,
				"value":       models.JSONB{"value": enabled},
				"enforcement": models.EntitlementEnforcementHard,
				"description": reReplyEntitlementDescription(key),
			}).Error; err != nil {
			return err
		}
	}
	return nil
}

func reReplyEntitlementDescription(key string) string {
	switch key {
	case "omnichannel.enabled":
		return "Shared omnichannel inbox and channel operations"
	case "crm.enabled":
		return "CRM pipeline, follow-ups, insights and automation"
	case "bookings.enabled":
		return "Appointment calendar and booking workflows"
	case "commerce.enabled":
		return "Packages, invoices and payment tracking"
	case "copilot.enabled":
		return "Reviewed Qwen AI Copilot workspace"
	case "threads.public_engagement.enabled":
		return "Threads public replies and mentions beta"
	default:
		return ""
	}
}
