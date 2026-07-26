package models

import (
	"github.com/google/uuid"
)

const (
	ResellerStatusActive    = "active"
	ResellerStatusSuspended = "suspended"

	ResellerPlanStarter    = "starter"
	ResellerPlanGrowth     = "growth"
	ResellerPlanEnterprise = "enterprise"

	ResellerRoleOwner = "owner"
	ResellerRoleAdmin = "admin"

	MembershipSourceDirect   = "direct"
	MembershipSourceReseller = "reseller"
)

// Reseller is a commercial partner that owns a portfolio of organizations.
// It is part of the control plane: customer CRM rows remain isolated by the
// organization-level PostgreSQL RLS policies.
type Reseller struct {
	BaseModel
	Name             string `gorm:"size:255;not null" json:"name"`
	Slug             string `gorm:"size:100;uniqueIndex;not null" json:"slug"`
	Status           string `gorm:"size:20;default:'active';index;not null" json:"status"`
	Plan             string `gorm:"size:30;default:'starter';not null" json:"plan"`
	MaxOrganizations int    `gorm:"default:10;not null" json:"max_organizations"`

	BrandName    string `gorm:"size:255" json:"brand_name"`
	LogoURL      string `gorm:"type:text" json:"logo_url"`
	PrimaryColor string `gorm:"size:20;default:'#0f766e'" json:"primary_color"`
	AccentColor  string `gorm:"size:20;default:'#f59e0b'" json:"accent_color"`
	SupportEmail string `gorm:"size:255" json:"support_email"`
	CustomDomain string `gorm:"size:255;index" json:"custom_domain"`

	Settings    JSONB      `gorm:"type:jsonb;default:'{}'" json:"settings"`
	CreatedByID *uuid.UUID `gorm:"type:uuid;index" json:"created_by_id,omitempty"`

	Organizations []Organization   `gorm:"foreignKey:ResellerID" json:"organizations,omitempty"`
	Members       []ResellerMember `gorm:"foreignKey:ResellerID" json:"members,omitempty"`
}

func (Reseller) TableName() string {
	return "resellers"
}

// ResellerMember grants a user control-plane access to one reseller
// portfolio. Customer-organization access is materialized as traceable
// UserOrganization rows so existing JWT, RBAC, and RLS behavior stays intact.
type ResellerMember struct {
	BaseModel
	ResellerID uuid.UUID `gorm:"type:uuid;uniqueIndex:idx_reseller_user;not null" json:"reseller_id"`
	UserID     uuid.UUID `gorm:"type:uuid;uniqueIndex:idx_reseller_user;not null" json:"user_id"`
	Role       string    `gorm:"size:20;default:'admin';not null" json:"role"`
	IsActive   bool      `gorm:"default:true;index" json:"is_active"`

	Reseller *Reseller `gorm:"foreignKey:ResellerID" json:"reseller,omitempty"`
	User     *User     `gorm:"foreignKey:UserID" json:"user,omitempty"`
}

func (ResellerMember) TableName() string {
	return "reseller_members"
}
