package models

import (
	"time"

	"github.com/google/uuid"
)

// MetaAnalyticsSnapshot stores provider-shaped analytics for a logical account
// that must be served locally. Snapshot accounts do not require a
// whatsapp_accounts row, which keeps demo fixtures independent from provider
// credentials and outbound Meta requests.
type MetaAnalyticsSnapshot struct {
	BaseModel
	OrganizationID uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:idx_meta_analytics_snapshot_identity,priority:1;index:idx_meta_analytics_snapshot_lookup,priority:1" json:"organization_id"`
	AccountID      uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:idx_meta_analytics_snapshot_identity,priority:2;index:idx_meta_analytics_snapshot_lookup,priority:2" json:"account_id"`
	AccountName    string    `gorm:"size:120;not null" json:"account_name"`
	PhoneID        string    `gorm:"size:100;not null;default:''" json:"phone_id"`
	Dataset        string    `gorm:"size:100;not null;default:'';index" json:"dataset"`
	AnalyticsType  string    `gorm:"size:40;not null;uniqueIndex:idx_meta_analytics_snapshot_identity,priority:3;index:idx_meta_analytics_snapshot_lookup,priority:3" json:"analytics_type"`
	Granularity    string    `gorm:"size:20;not null;uniqueIndex:idx_meta_analytics_snapshot_identity,priority:4;index:idx_meta_analytics_snapshot_lookup,priority:4" json:"granularity"`
	PeriodStart    time.Time `gorm:"not null;uniqueIndex:idx_meta_analytics_snapshot_identity,priority:5;index:idx_meta_analytics_snapshot_period,priority:1" json:"period_start"`
	PeriodEnd      time.Time `gorm:"not null;uniqueIndex:idx_meta_analytics_snapshot_identity,priority:6;index:idx_meta_analytics_snapshot_period,priority:2" json:"period_end"`
	Payload        JSONB     `gorm:"type:jsonb;not null;default:'{}'" json:"payload"`
	TemplateNames  JSONB     `gorm:"type:jsonb;not null;default:'{}'" json:"template_names"`
	IsMock         bool      `gorm:"not null;default:true;index" json:"is_mock"`
}

func (MetaAnalyticsSnapshot) TableName() string {
	return "meta_analytics_snapshots"
}
