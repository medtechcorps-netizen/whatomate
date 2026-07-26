package access

import (
	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

var resellerAdminRoles = []string{
	models.ResellerRoleOwner,
	models.ResellerRoleAdmin,
}

// ResellerDerivedMembershipActive validates that a materialized organization
// membership is still backed by an active reseller administrator assignment.
// Direct memberships remain independent from the reseller control plane.
func ResellerDerivedMembershipActive(db *gorm.DB, membership *models.UserOrganization) bool {
	if db == nil || membership == nil {
		return false
	}
	if membership.Source == "" || membership.Source == models.MembershipSourceDirect {
		return true
	}
	if membership.Source != models.MembershipSourceReseller || membership.ResellerMemberID == nil {
		return false
	}

	var count int64
	err := db.Table("reseller_members").
		Joins("JOIN resellers ON resellers.id = reseller_members.reseller_id AND resellers.deleted_at IS NULL").
		Joins("JOIN organizations ON organizations.reseller_id = reseller_members.reseller_id AND organizations.deleted_at IS NULL").
		Where(`reseller_members.id = ? AND reseller_members.user_id = ?
			AND organizations.id = ? AND reseller_members.is_active = ?
			AND reseller_members.deleted_at IS NULL AND reseller_members.role IN ?
			AND resellers.status = ?`,
			*membership.ResellerMemberID, membership.UserID, membership.OrganizationID, true,
			resellerAdminRoles, models.ResellerStatusActive).
		Count(&count).Error
	return err == nil && count > 0
}

// OrganizationMembership returns an organization membership only when the
// membership's source is still valid.
func OrganizationMembership(db *gorm.DB, userID, orgID uuid.UUID) (*models.UserOrganization, bool) {
	var membership models.UserOrganization
	if db == nil || userID == uuid.Nil || orgID == uuid.Nil {
		return nil, false
	}
	if err := db.Where(
		"user_id = ? AND organization_id = ?", userID, orgID,
	).First(&membership).Error; err != nil {
		return nil, false
	}
	return &membership, ResellerDerivedMembershipActive(db, &membership)
}

// IsResellerAdmin reports whether the user manages at least one active
// reseller. Platform super administrators are intentionally not included.
func IsResellerAdmin(db *gorm.DB, userID uuid.UUID) bool {
	if db == nil || userID == uuid.Nil {
		return false
	}
	var count int64
	err := db.Table("reseller_members").
		Joins("JOIN resellers ON resellers.id = reseller_members.reseller_id AND resellers.deleted_at IS NULL").
		Where(`reseller_members.user_id = ? AND reseller_members.is_active = ?
			AND reseller_members.deleted_at IS NULL AND reseller_members.role IN ?
			AND resellers.status = ?`,
			userID, true, resellerAdminRoles, models.ResellerStatusActive).
		Count(&count).Error
	return err == nil && count > 0
}

// IsResellerAdminForOrganization validates that the organization belongs to an
// active reseller portfolio managed by the user.
func IsResellerAdminForOrganization(db *gorm.DB, userID, orgID uuid.UUID) bool {
	if db == nil || userID == uuid.Nil || orgID == uuid.Nil {
		return false
	}
	var count int64
	err := db.Table("reseller_members").
		Joins("JOIN resellers ON resellers.id = reseller_members.reseller_id AND resellers.deleted_at IS NULL").
		Joins("JOIN organizations ON organizations.reseller_id = reseller_members.reseller_id AND organizations.deleted_at IS NULL").
		Where(`reseller_members.user_id = ? AND organizations.id = ?
			AND reseller_members.is_active = ? AND reseller_members.deleted_at IS NULL
			AND reseller_members.role IN ? AND resellers.status = ?`,
			userID, orgID, true, resellerAdminRoles, models.ResellerStatusActive).
		Count(&count).Error
	return err == nil && count > 0
}
