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
	active, err := ResellerDerivedMembershipActiveChecked(db, membership)
	return err == nil && active
}

// ResellerDerivedMembershipActiveChecked preserves unavailable authority
// evidence separately from a confirmed missing or inactive assignment.
func ResellerDerivedMembershipActiveChecked(db *gorm.DB, membership *models.UserOrganization) (bool, error) {
	if db == nil {
		return false, gorm.ErrInvalidDB
	}
	if membership == nil {
		return false, nil
	}
	if membership.Source == "" || membership.Source == models.MembershipSourceDirect {
		return true, nil
	}
	if membership.Source != models.MembershipSourceReseller || membership.ResellerMemberID == nil {
		return false, nil
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
	return count > 0 && err == nil, err
}

// OrganizationMembership returns an organization membership only when the
// membership's source is still valid.
func OrganizationMembership(db *gorm.DB, userID, orgID uuid.UUID) (*models.UserOrganization, bool) {
	membership, active, err := OrganizationMembershipChecked(db, userID, orgID)
	return membership, err == nil && active
}

// OrganizationMembershipChecked distinguishes absence from a failed query.
func OrganizationMembershipChecked(db *gorm.DB, userID, orgID uuid.UUID) (*models.UserOrganization, bool, error) {
	if db == nil {
		return nil, false, gorm.ErrInvalidDB
	}
	if userID == uuid.Nil || orgID == uuid.Nil {
		return nil, false, nil
	}
	var memberships []models.UserOrganization
	if err := db.Where(
		"user_id = ? AND organization_id = ?", userID, orgID,
	).Limit(1).Find(&memberships).Error; err != nil {
		return nil, false, err
	}
	if len(memberships) == 0 {
		return nil, false, nil
	}
	active, err := ResellerDerivedMembershipActiveChecked(db, &memberships[0])
	return &memberships[0], active, err
}

// IsResellerAdmin reports whether the user manages at least one active
// reseller. Platform super administrators are intentionally not included.
func IsResellerAdmin(db *gorm.DB, userID uuid.UUID) bool {
	admin, err := IsResellerAdminChecked(db, userID)
	return err == nil && admin
}

// IsResellerAdminChecked never turns an unavailable role lookup into a
// successful false role in an authenticated response.
func IsResellerAdminChecked(db *gorm.DB, userID uuid.UUID) (bool, error) {
	if db == nil {
		return false, gorm.ErrInvalidDB
	}
	if userID == uuid.Nil {
		return false, nil
	}
	var count int64
	err := db.Table("reseller_members").
		Joins("JOIN resellers ON resellers.id = reseller_members.reseller_id AND resellers.deleted_at IS NULL").
		Where(`reseller_members.user_id = ? AND reseller_members.is_active = ?
			AND reseller_members.deleted_at IS NULL AND reseller_members.role IN ?
			AND resellers.status = ?`,
			userID, true, resellerAdminRoles, models.ResellerStatusActive).
		Count(&count).Error
	return count > 0 && err == nil, err
}

// IsResellerAdminForOrganization validates that the organization belongs to an
// active reseller portfolio managed by the user.
func IsResellerAdminForOrganization(db *gorm.DB, userID, orgID uuid.UUID) bool {
	admin, err := IsResellerAdminForOrganizationChecked(db, userID, orgID)
	return err == nil && admin
}

func IsResellerAdminForOrganizationChecked(db *gorm.DB, userID, orgID uuid.UUID) (bool, error) {
	if db == nil {
		return false, gorm.ErrInvalidDB
	}
	if userID == uuid.Nil || orgID == uuid.Nil {
		return false, nil
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
	return count > 0 && err == nil, err
}
