package models

import (
	"time"

	"github.com/google/uuid"
)

const (
	ThreadsManagementModeWorkspaceBYO    = "workspace_byo"
	ThreadsManagementModePlatformManaged = "platform_managed"

	ThreadsPlatformBindingStatusPending     = "pending"
	ThreadsPlatformBindingStatusActive      = "active"
	ThreadsPlatformBindingStatusQuarantined = "quarantined"
	ThreadsPlatformBindingStatusRevoked     = "revoked"

	ThreadsPlatformEventWebhook         = "webhook"
	ThreadsPlatformEventDeauthorization = "deauthorization"
	ThreadsPlatformEventDataDeletion    = "data_deletion"

	ThreadsPlatformRoutingReceived    = "received"
	ThreadsPlatformRoutingRouted      = "routed"
	ThreadsPlatformRoutingUnknown     = "unknown"
	ThreadsPlatformRoutingAmbiguous   = "ambiguous"
	ThreadsPlatformRoutingQuarantined = "quarantined"
	ThreadsPlatformRoutingProcessed   = "processed"
)

// ThreadsPlatformBinding is the authoritative claim from one managed Threads
// app-scoped subject and one Threads profile/asset to a tenant integration.
// Rows remain tenant protected by PostgreSQL RLS, while partial unique indexes
// enforce ownership globally across every tenant. Tokens and platform secrets
// never belong in this table.
type ThreadsPlatformBinding struct {
	BaseModel
	OrganizationID          uuid.UUID  `gorm:"type:uuid;not null;index" json:"organization_id"`
	IntegrationID           uuid.UUID  `gorm:"type:uuid;not null;index" json:"integration_id"`
	ChannelAccountID        *uuid.UUID `gorm:"type:uuid;index" json:"channel_account_id,omitempty"`
	PlatformAppKey          string     `gorm:"size:64;not null;index" json:"platform_app_key"`
	PlatformAppID           string     `gorm:"size:64;not null" json:"platform_app_id"`
	OAuthSubjectID          string     `gorm:"column:oauth_subject_id;size:128;not null" json:"-"`
	AuthorityAssetID        string     `gorm:"size:128;not null" json:"-"`
	ConfigurationGeneration uint64     `gorm:"not null" json:"configuration_generation"`
	AuthorizationGeneration uint64     `gorm:"not null;default:1" json:"authorization_generation"`
	Status                  string     `gorm:"size:24;not null;default:pending;index" json:"status"`
	ClaimedAt               time.Time  `gorm:"not null" json:"claimed_at"`
	ReleasedAt              *time.Time `json:"released_at,omitempty"`
	ReleaseReasonCode       string     `gorm:"size:80" json:"release_reason_code,omitempty"`
}

func (ThreadsPlatformBinding) TableName() string {
	return "threads_platform_bindings"
}

// ThreadsPlatformEventJournal is a global, non-secret receipt journal used
// before a tenant can be resolved. It intentionally stores only SHA-256
// digests and bounded state codes: never raw payloads, OAuth subjects, tokens,
// app secrets, provider error text, or clinic data.
type ThreadsPlatformEventJournal struct {
	BaseModel
	PlatformAppKey          string     `gorm:"size:64;not null;index:idx_threads_platform_journal_app_received,priority:1;uniqueIndex:uq_threads_platform_journal_event,priority:1" json:"platform_app_key"`
	PlatformAppID           string     `gorm:"size:64;not null" json:"platform_app_id"`
	EventDigest             string     `gorm:"size:64;not null;uniqueIndex:uq_threads_platform_journal_event,priority:2" json:"event_digest"`
	EventType               string     `gorm:"size:32;not null;index" json:"event_type"`
	SubjectDigest           string     `gorm:"size:64;not null" json:"subject_digest"`
	ConfigurationGeneration uint64     `gorm:"not null" json:"configuration_generation"`
	RoutingState            string     `gorm:"size:24;not null;default:received;index" json:"routing_state"`
	ReasonCode              string     `gorm:"size:80" json:"reason_code,omitempty"`
	ReceivedAt              time.Time  `gorm:"not null;index:idx_threads_platform_journal_app_received,priority:2" json:"received_at"`
	ProcessedAt             *time.Time `json:"processed_at,omitempty"`
}

func (ThreadsPlatformEventJournal) TableName() string {
	return "threads_platform_event_journal"
}
