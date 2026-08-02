package models

import (
	"time"

	"github.com/google/uuid"
)

// ProviderIntegration stores tenant-specific Integration Center state that is
// not already owned by a provider's runtime model. Meta credentials remain in
// Organization.Settings and Qwen credentials remain in CopilotSettings; this
// record supplies shared enablement/health metadata and is the source of truth
// for pre-approval Threads and TikTok configuration.
//
// CredentialData contains only individually encrypted values and is excluded
// from JSON so an accidental model response cannot disclose credentials.
type ProviderIntegration struct {
	BaseModel
	OrganizationID       uuid.UUID  `gorm:"type:uuid;not null;index;uniqueIndex:idx_provider_integrations_org_provider,priority:1" json:"organization_id"`
	Provider             string     `gorm:"size:32;not null;uniqueIndex:idx_provider_integrations_org_provider,priority:2" json:"provider"`
	Enabled              bool       `gorm:"not null;default:false" json:"enabled"`
	Config               JSONB      `gorm:"type:jsonb;not null;default:'{}'" json:"config"`
	CredentialData       JSONB      `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	CredentialsUpdatedAt *time.Time `json:"credentials_updated_at,omitempty"`
	LastTestedAt         *time.Time `json:"last_tested_at,omitempty"`
	LastSuccessfulAt     *time.Time `json:"last_successful_at,omitempty"`
	LastErrorCode        string     `gorm:"size:80" json:"last_error_code,omitempty"`
	LastErrorMessage     string     `gorm:"size:500" json:"last_error_message,omitempty"`
	ValidationToken      string     `gorm:"size:36" json:"-"`
	CreatedByID          *uuid.UUID `gorm:"type:uuid;index" json:"created_by_id,omitempty"`
	UpdatedByID          *uuid.UUID `gorm:"type:uuid;index" json:"updated_by_id,omitempty"`

	Organization *Organization `gorm:"foreignKey:OrganizationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"organization,omitempty"`
	CreatedBy    *User         `gorm:"foreignKey:CreatedByID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"created_by,omitempty"`
	UpdatedBy    *User         `gorm:"foreignKey:UpdatedByID;constraint:OnUpdate:CASCADE,OnDelete:SET NULL" json:"updated_by,omitempty"`
}

func (ProviderIntegration) TableName() string {
	return "provider_integrations"
}
