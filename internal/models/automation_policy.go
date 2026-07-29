package models

import (
	"time"

	"github.com/google/uuid"
)

// AutomationPolicyStatus is the editable policy lifecycle. Published versions
// remain immutable when a policy is paused or a newer version is activated.
type AutomationPolicyStatus string

const (
	AutomationPolicyStatusDraft    AutomationPolicyStatus = "draft"
	AutomationPolicyStatusActive   AutomationPolicyStatus = "active"
	AutomationPolicyStatusPaused   AutomationPolicyStatus = "paused"
	AutomationPolicyStatusArchived AutomationPolicyStatus = "archived"
)

// AutomationExecutionStatus is the durable lifecycle for one policy/event run.
type AutomationExecutionStatus string

const (
	AutomationExecutionStatusPending    AutomationExecutionStatus = "pending"
	AutomationExecutionStatusProcessing AutomationExecutionStatus = "processing"
	AutomationExecutionStatusCompleted  AutomationExecutionStatus = "completed"
	AutomationExecutionStatusSkipped    AutomationExecutionStatus = "skipped"
	AutomationExecutionStatusFailed     AutomationExecutionStatus = "failed"
	AutomationExecutionStatusCancelled  AutomationExecutionStatus = "cancelled"
)

// AutomationExecutionStepStatus is the lifecycle of one evaluated graph node.
type AutomationExecutionStepStatus string

const (
	AutomationExecutionStepStatusPending    AutomationExecutionStepStatus = "pending"
	AutomationExecutionStepStatusProcessing AutomationExecutionStepStatus = "processing"
	AutomationExecutionStepStatusCompleted  AutomationExecutionStepStatus = "completed"
	AutomationExecutionStepStatusSkipped    AutomationExecutionStepStatus = "skipped"
	AutomationExecutionStepStatusFailed     AutomationExecutionStepStatus = "failed"
	AutomationExecutionStepStatusCancelled  AutomationExecutionStepStatus = "cancelled"
)

// AutomationEventReceiptStatus is the durable fan-out lifecycle for one
// append-only customer activity. Receipts are created by a database trigger in
// the same transaction as the activity.
type AutomationEventReceiptStatus string

const (
	AutomationEventReceiptStatusPending    AutomationEventReceiptStatus = "pending"
	AutomationEventReceiptStatusProcessing AutomationEventReceiptStatus = "processing"
	AutomationEventReceiptStatusCompleted  AutomationEventReceiptStatus = "completed"
	AutomationEventReceiptStatusFailed     AutomationEventReceiptStatus = "failed"
)

// AutomationPolicy is the editable policy head. DraftGraph can change while an
// older immutable ActiveVersionID continues to execute.
type AutomationPolicy struct {
	BaseModel
	OrganizationID      uuid.UUID              `gorm:"type:uuid;not null;index" json:"organization_id"`
	Name                string                 `gorm:"size:150;not null" json:"name"`
	Description         string                 `gorm:"type:text" json:"description,omitempty"`
	Status              AutomationPolicyStatus `gorm:"size:20;not null;default:'draft';index" json:"status"`
	DraftGraph          JSONB                  `gorm:"type:jsonb;not null;default:'{}'" json:"graph"`
	TriggerEventTypes   JSONBArray             `gorm:"type:jsonb;not null;default:'[]';index:,type:gin" json:"trigger_event_types"`
	ActiveVersionID     *uuid.UUID             `gorm:"type:uuid;index" json:"active_version_id,omitempty"`
	ActiveVersionNumber int                    `gorm:"not null;default:0" json:"active_version_number"`
	ActivatedAt         *time.Time             `gorm:"index" json:"activated_at,omitempty"`
	PausedAt            *time.Time             `json:"paused_at,omitempty"`
	Version             int64                  `gorm:"not null;default:1" json:"version"`
	CreatedByID         *uuid.UUID             `gorm:"type:uuid" json:"created_by_id,omitempty"`
	UpdatedByID         *uuid.UUID             `gorm:"type:uuid" json:"updated_by_id,omitempty"`
}

func (AutomationPolicy) TableName() string {
	return "automation_policies"
}

// AutomationPolicyVersion is an append-only activated graph snapshot.
type AutomationPolicyVersion struct {
	ID                uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	OrganizationID    uuid.UUID  `gorm:"type:uuid;not null;index" json:"organization_id"`
	PolicyID          uuid.UUID  `gorm:"type:uuid;not null;index" json:"policy_id"`
	Number            int        `gorm:"not null" json:"number"`
	TriggerEventTypes JSONBArray `gorm:"type:jsonb;not null;default:'[]';index:,type:gin" json:"trigger_event_types"`
	Graph             JSONB      `gorm:"type:jsonb;not null;default:'{}'" json:"graph"`
	Checksum          string     `gorm:"size:64;not null" json:"checksum"`
	CreatedByID       *uuid.UUID `gorm:"type:uuid" json:"created_by_id,omitempty"`
	PublishedAt       time.Time  `gorm:"not null;index" json:"published_at"`
	CreatedAt         time.Time  `gorm:"not null;autoCreateTime" json:"created_at"`
}

func (AutomationPolicyVersion) TableName() string {
	return "automation_policy_versions"
}

// AutomationPolicyActivation records the exact interval in which a version
// owned matching customer events. Closing an interval does not cancel work
// triggered while it was open.
type AutomationPolicyActivation struct {
	ID                  uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	OrganizationID      uuid.UUID  `gorm:"type:uuid;not null;index" json:"organization_id"`
	PolicyID            uuid.UUID  `gorm:"type:uuid;not null;index" json:"policy_id"`
	PolicyVersionID     uuid.UUID  `gorm:"type:uuid;not null;index" json:"policy_version_id"`
	PolicyVersionNumber int        `gorm:"not null" json:"policy_version_number"`
	ActiveFrom          time.Time  `gorm:"not null;index" json:"active_from"`
	ActiveUntil         *time.Time `gorm:"index" json:"active_until,omitempty"`
	CreatedByID         *uuid.UUID `gorm:"type:uuid" json:"created_by_id,omitempty"`
	ClosedByID          *uuid.UUID `gorm:"type:uuid" json:"closed_by_id,omitempty"`
	CreatedAt           time.Time  `gorm:"not null;autoCreateTime" json:"created_at"`
	UpdatedAt           time.Time  `gorm:"not null;autoUpdateTime" json:"updated_at"`
}

func (AutomationPolicyActivation) TableName() string {
	return "automation_policy_activations"
}

// AutomationExecution pins an immutable policy version to one activity event.
type AutomationExecution struct {
	ID                  uuid.UUID                 `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	OrganizationID      uuid.UUID                 `gorm:"type:uuid;not null;index" json:"organization_id"`
	PolicyID            uuid.UUID                 `gorm:"type:uuid;not null;index" json:"policy_id"`
	PolicyVersionID     uuid.UUID                 `gorm:"type:uuid;not null;index" json:"policy_version_id"`
	PolicyVersionNumber int                       `gorm:"not null" json:"policy_version_number"`
	ActivityEventID     uuid.UUID                 `gorm:"type:uuid;not null;index" json:"activity_event_id"`
	ContactID           uuid.UUID                 `gorm:"type:uuid;not null;index" json:"contact_id"`
	Status              AutomationExecutionStatus `gorm:"size:20;not null;default:'pending';index" json:"status"`
	TriggeredAt         time.Time                 `gorm:"not null;index" json:"triggered_at"`
	StartedAt           *time.Time                `json:"started_at,omitempty"`
	CompletedAt         *time.Time                `json:"completed_at,omitempty"`
	LastError           string                    `gorm:"type:text" json:"last_error,omitempty"`
	Context             JSONB                     `gorm:"type:jsonb;not null;default:'{}'" json:"context"`
	Result              JSONB                     `gorm:"type:jsonb;not null;default:'{}'" json:"result"`
	CreatedAt           time.Time                 `gorm:"not null;autoCreateTime" json:"created_at"`
	UpdatedAt           time.Time                 `gorm:"not null;autoUpdateTime" json:"updated_at"`
}

func (AutomationExecution) TableName() string {
	return "automation_executions"
}

// AutomationExecutionStep records the deterministic outcome of each graph
// node. Action steps also own the durable per-node idempotency key.
type AutomationExecutionStep struct {
	ID             uuid.UUID                     `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	OrganizationID uuid.UUID                     `gorm:"type:uuid;not null;index" json:"organization_id"`
	ExecutionID    uuid.UUID                     `gorm:"type:uuid;not null;index" json:"execution_id"`
	NodeID         string                        `gorm:"size:100;not null" json:"node_id"`
	NodeType       string                        `gorm:"size:40;not null" json:"node_type"`
	Status         AutomationExecutionStepStatus `gorm:"size:20;not null;default:'pending';index" json:"status"`
	ScheduledAt    *time.Time                    `gorm:"index" json:"scheduled_at,omitempty"`
	StartedAt      *time.Time                    `json:"started_at,omitempty"`
	CompletedAt    *time.Time                    `json:"completed_at,omitempty"`
	TaskID         *uuid.UUID                    `gorm:"type:uuid;index" json:"task_id,omitempty"`
	Output         JSONB                         `gorm:"type:jsonb;not null;default:'{}'" json:"output"`
	LastError      string                        `gorm:"type:text" json:"last_error,omitempty"`
	IdempotencyKey string                        `gorm:"size:255;not null" json:"idempotency_key"`
	CreatedAt      time.Time                     `gorm:"not null;autoCreateTime" json:"created_at"`
	UpdatedAt      time.Time                     `gorm:"not null;autoUpdateTime" json:"updated_at"`
}

func (AutomationExecutionStep) TableName() string {
	return "automation_execution_steps"
}

// AutomationEventReceipt is created atomically for every customer activity.
// It avoids timestamp/cursor races caused by transactions committing out of
// order and gives the automation dispatcher its own lease lifecycle.
type AutomationEventReceipt struct {
	ID              uuid.UUID                    `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	OrganizationID  uuid.UUID                    `gorm:"type:uuid;not null;index" json:"organization_id"`
	ActivityEventID uuid.UUID                    `gorm:"type:uuid;not null;index" json:"activity_event_id"`
	Status          AutomationEventReceiptStatus `gorm:"size:20;not null;default:'pending';index" json:"status"`
	Attempts        int                          `gorm:"not null;default:0" json:"attempts"`
	MaxAttempts     int                          `gorm:"not null;default:10" json:"max_attempts"`
	IngestedAt      time.Time                    `gorm:"not null;index" json:"ingested_at"`
	AvailableAt     time.Time                    `gorm:"not null;index" json:"available_at"`
	LockedAt        *time.Time                   `gorm:"index" json:"locked_at,omitempty"`
	LockedBy        string                       `gorm:"size:255" json:"locked_by,omitempty"`
	CompletedAt     *time.Time                   `json:"completed_at,omitempty"`
	LastError       string                       `gorm:"type:text" json:"last_error,omitempty"`
	CreatedAt       time.Time                    `gorm:"not null;autoCreateTime;index" json:"created_at"`
	UpdatedAt       time.Time                    `gorm:"not null;autoUpdateTime" json:"updated_at"`
}

func (AutomationEventReceipt) TableName() string {
	return "automation_event_receipts"
}

// AutomationDispatchState is a one-row-per-tenant serialization lock shared
// by activation and receipt fan-out. It closes the activation/dispatch ordering
// race without imposing global cross-tenant serialization.
type AutomationDispatchState struct {
	OrganizationID uuid.UUID `gorm:"type:uuid;primaryKey" json:"organization_id"`
	UpdatedAt      time.Time `gorm:"not null;autoUpdateTime" json:"updated_at"`
}

func (AutomationDispatchState) TableName() string {
	return "automation_dispatch_states"
}
