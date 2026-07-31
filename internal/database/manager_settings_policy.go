package database

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ManagerSettingsPolicyVersionKey is stored in the organization settings JSON.
// A per-organization marker is intentional: it lets this migration remove the
// legacy defaults exactly once without undoing a later explicit super-admin
// grant on subsequent application starts.
const ManagerSettingsPolicyVersionKey = "system_manager_settings_policy_version"

const ManagerSettingsPolicyVersion = 1

var retiredManagerSettingsPermissions = map[string]struct{}{
	"settings.general:write": {},
	"webhooks:read":          {},
	"webhooks:write":         {},
	"webhooks:delete":        {},
	"custom_actions:read":    {},
	"custom_actions:write":   {},
	"custom_actions:delete":  {},
	"privacy.settings:read":  {},
	"privacy.settings:write": {},
	"privacy.requests:read":  {},
	"privacy.requests:write": {},
	"support:read":           {},
	"support:write":          {},
}

// ApplyManagerSettingsPolicyMigration removes legacy, sensitive Settings
// permissions from each organization's system manager role once. Custom roles,
// non-manager system roles, and all other permissions are deliberately ignored.
func ApplyManagerSettingsPolicyMigration(db *gorm.DB) error {
	var organizationIDs []uuid.UUID
	if err := db.Model(&models.Organization{}).Pluck("id", &organizationIDs).Error; err != nil {
		return fmt.Errorf("list organizations for manager settings policy: %w", err)
	}

	for _, organizationID := range organizationIDs {
		if err := applyManagerSettingsPolicyForOrg(db, organizationID); err != nil {
			return fmt.Errorf(
				"apply manager settings policy for organization %s: %w",
				organizationID,
				err,
			)
		}
	}
	return nil
}

func applyManagerSettingsPolicyForOrg(db *gorm.DB, organizationID uuid.UUID) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var organization models.Organization
		if err := tx.
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", organizationID).
			First(&organization).Error; err != nil {
			return err
		}
		if managerSettingsPolicyVersion(organization.Settings) >= ManagerSettingsPolicyVersion {
			return nil
		}

		var managerRole models.CustomRole
		if err := tx.Where(
			"organization_id = ? AND name = ? AND is_system = ?",
			organizationID,
			"manager",
			true,
		).First(&managerRole).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return markManagerSettingsPolicyVersion(tx, &organization)
			}
			return err
		}

		var candidatePermissions []models.Permission
		if err := tx.Where("resource IN ?", []string{
			models.ResourceSettingsGeneral,
			models.ResourceWebhooks,
			models.ResourceCustomActions,
			models.ResourcePrivacySettings,
			models.ResourcePrivacyRequests,
			models.ResourceSupport,
		}).Find(&candidatePermissions).Error; err != nil {
			return err
		}

		permissionIDs := make([]uuid.UUID, 0, len(candidatePermissions))
		for _, permission := range candidatePermissions {
			key := permission.Resource + ":" + permission.Action
			if _, retired := retiredManagerSettingsPermissions[key]; retired {
				permissionIDs = append(permissionIDs, permission.ID)
			}
		}
		if len(permissionIDs) > 0 {
			if err := tx.Where(
				"custom_role_id = ? AND permission_id IN ?",
				managerRole.ID,
				permissionIDs,
			).Delete(&models.RolePermission{}).Error; err != nil {
				return err
			}
		}

		return markManagerSettingsPolicyVersion(tx, &organization)
	})
}

func markManagerSettingsPolicyVersion(db *gorm.DB, organization *models.Organization) error {
	if organization.Settings == nil {
		organization.Settings = models.JSONB{}
	}
	organization.Settings[ManagerSettingsPolicyVersionKey] = ManagerSettingsPolicyVersion
	return db.Model(&models.Organization{}).
		Where("id = ?", organization.ID).
		Update("settings", organization.Settings).Error
}

func managerSettingsPolicyVersion(settings models.JSONB) int {
	if settings == nil {
		return 0
	}
	value, exists := settings[ManagerSettingsPolicyVersionKey]
	if !exists {
		return 0
	}
	switch typed := value.(type) {
	case int:
		return typed
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		version, _ := typed.Int64()
		return int(version)
	case string:
		version, _ := strconv.Atoi(typed)
		return version
	default:
		return 0
	}
}
