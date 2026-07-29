package models

import (
	"time"

	"github.com/google/uuid"
)

// ScheduledJobStatus is the durable lifecycle of a delayed background job.
type ScheduledJobStatus string

const (
	ScheduledJobStatusPending    ScheduledJobStatus = "pending"
	ScheduledJobStatusProcessing ScheduledJobStatus = "processing"
	ScheduledJobStatusCompleted  ScheduledJobStatus = "completed"
	ScheduledJobStatusFailed     ScheduledJobStatus = "failed"
	ScheduledJobStatusCancelled  ScheduledJobStatus = "cancelled"
)

// OutboxEventStatus is the delivery lifecycle of a transactional outbox event.
type OutboxEventStatus string

const (
	OutboxEventStatusPending    OutboxEventStatus = "pending"
	OutboxEventStatusProcessing OutboxEventStatus = "processing"
	OutboxEventStatusPublished  OutboxEventStatus = "published"
	OutboxEventStatusFailed     OutboxEventStatus = "failed"
	OutboxEventStatusCancelled  OutboxEventStatus = "cancelled"
)

// CRMPipelineStageKind describes whether a stage is active or terminal.
type CRMPipelineStageKind string

const (
	CRMPipelineStageKindOpen CRMPipelineStageKind = "open"
	CRMPipelineStageKindWon  CRMPipelineStageKind = "won"
	CRMPipelineStageKindLost CRMPipelineStageKind = "lost"
)

// CRMLeadStatus is the normalized lifecycle status of a CRM lead.
type CRMLeadStatus string

const (
	CRMLeadStatusOpen     CRMLeadStatus = "open"
	CRMLeadStatusWon      CRMLeadStatus = "won"
	CRMLeadStatusLost     CRMLeadStatus = "lost"
	CRMLeadStatusArchived CRMLeadStatus = "archived"
)

// CRMLeadSource identifies where a lead originated.
type CRMLeadSource string

const (
	CRMLeadSourceWhatsApp CRMLeadSource = "whatsapp"
	CRMLeadSourceCampaign CRMLeadSource = "campaign"
	CRMLeadSourceReferral CRMLeadSource = "referral"
	CRMLeadSourceWalkIn   CRMLeadSource = "walk_in"
	CRMLeadSourceAPI      CRMLeadSource = "api"
	CRMLeadSourceImport   CRMLeadSource = "import"
	CRMLeadSourceOther    CRMLeadSource = "other"
)

// CustomerActivityCategory groups timeline events without weakening the
// stable, typed event name used by automations and webhook consumers.
type CustomerActivityCategory string

const (
	CustomerActivityCategoryContact CustomerActivityCategory = "contact"
	CustomerActivityCategoryMessage CustomerActivityCategory = "message"
	CustomerActivityCategoryCRM     CustomerActivityCategory = "crm"
	CustomerActivityCategoryTask    CustomerActivityCategory = "task"
	CustomerActivityCategoryBooking CustomerActivityCategory = "booking"
	CustomerActivityCategoryPackage CustomerActivityCategory = "package"
	CustomerActivityCategoryInvoice CustomerActivityCategory = "invoice"
	CustomerActivityCategoryPayment CustomerActivityCategory = "payment"
	CustomerActivityCategoryConsent CustomerActivityCategory = "consent"
)

// CustomerActivityEventType is the canonical customer-lifecycle event name.
// Values are additive API contracts: existing values must never be repurposed.
type CustomerActivityEventType string

const (
	CustomerActivityContactCreated  CustomerActivityEventType = "contact.created"
	CustomerActivityContactUpdated  CustomerActivityEventType = "contact.updated"
	CustomerActivityContactMerged   CustomerActivityEventType = "contact.merged"
	CustomerActivityMessageIncoming CustomerActivityEventType = "message.incoming"
	CustomerActivityCRMLeadCreated  CustomerActivityEventType = "crm.lead.created"
	CustomerActivityCRMLeadUpdated  CustomerActivityEventType = "crm.lead.updated"
	CustomerActivityCRMStageMoved   CustomerActivityEventType = "crm.lead.stage_moved"
	CustomerActivityTaskCreated     CustomerActivityEventType = "task.created"
	CustomerActivityTaskCompleted   CustomerActivityEventType = "task.completed"
	CustomerActivityBookingCreated  CustomerActivityEventType = "booking.created"
	CustomerActivityBookingStatus   CustomerActivityEventType = "booking.status_changed"
	CustomerActivityPackageSold     CustomerActivityEventType = "package.sold"
	CustomerActivityPackageLow      CustomerActivityEventType = "package.balance_low"
	CustomerActivityPackageExpiring CustomerActivityEventType = "package.expiring"
	CustomerActivityInvoiceCreated  CustomerActivityEventType = "invoice.created"
	CustomerActivityInvoiceOverdue  CustomerActivityEventType = "invoice.overdue"
	CustomerActivityInvoicePaid     CustomerActivityEventType = "invoice.paid"
	CustomerActivityPaymentRecorded CustomerActivityEventType = "payment.recorded"
	CustomerActivityConsentOptedOut CustomerActivityEventType = "consent.opted_out"
)

// CustomerActivityActorType identifies who caused a lifecycle event.
type CustomerActivityActorType string

const (
	CustomerActivityActorUser     CustomerActivityActorType = "user"
	CustomerActivityActorContact  CustomerActivityActorType = "contact"
	CustomerActivityActorSystem   CustomerActivityActorType = "system"
	CustomerActivityActorProvider CustomerActivityActorType = "provider"
	CustomerActivityActorImport   CustomerActivityActorType = "import"
)

// FollowUpTaskStatus is the workflow status of a follow-up task.
type FollowUpTaskStatus string

const (
	FollowUpTaskStatusOpen       FollowUpTaskStatus = "open"
	FollowUpTaskStatusInProgress FollowUpTaskStatus = "in_progress"
	FollowUpTaskStatusCompleted  FollowUpTaskStatus = "completed"
	FollowUpTaskStatusCancelled  FollowUpTaskStatus = "cancelled"
)

// FollowUpTaskPriority is the urgency assigned to a follow-up task.
type FollowUpTaskPriority string

const (
	FollowUpTaskPriorityLow    FollowUpTaskPriority = "low"
	FollowUpTaskPriorityNormal FollowUpTaskPriority = "normal"
	FollowUpTaskPriorityHigh   FollowUpTaskPriority = "high"
	FollowUpTaskPriorityUrgent FollowUpTaskPriority = "urgent"
)

// BookingServiceKind distinguishes one-to-one appointments from group classes.
type BookingServiceKind string

const (
	BookingServiceKindAppointment BookingServiceKind = "appointment"
	BookingServiceKindClass       BookingServiceKind = "class"
)

// BookingResourceKind classifies a schedulable resource.
type BookingResourceKind string

const (
	BookingResourceKindPractitioner BookingResourceKind = "practitioner"
	BookingResourceKindInstructor   BookingResourceKind = "instructor"
	BookingResourceKindRoom         BookingResourceKind = "room"
	BookingResourceKindEquipment    BookingResourceKind = "equipment"
)

// BookingEventStatus is the lifecycle of a calendar event.
type BookingEventStatus string

const (
	BookingEventStatusScheduled BookingEventStatus = "scheduled"
	BookingEventStatusCompleted BookingEventStatus = "completed"
	BookingEventStatusCancelled BookingEventStatus = "cancelled"
)

// BookingStatus is the attendee-level lifecycle of a booking.
type BookingStatus string

const (
	BookingStatusReserved   BookingStatus = "reserved"
	BookingStatusConfirmed  BookingStatus = "confirmed"
	BookingStatusWaitlisted BookingStatus = "waitlisted"
	BookingStatusCheckedIn  BookingStatus = "checked_in"
	BookingStatusCompleted  BookingStatus = "completed"
	BookingStatusNoShow     BookingStatus = "no_show"
	BookingStatusCancelled  BookingStatus = "cancelled"
)

// BookingSource identifies how a booking entered the system.
type BookingSource string

const (
	BookingSourceAgent    BookingSource = "agent"
	BookingSourceWhatsApp BookingSource = "whatsapp"
	BookingSourceAPI      BookingSource = "api"
	BookingSourceImport   BookingSource = "import"
)

// ContactPackageStatus is the lifecycle of a package owned by a contact.
type ContactPackageStatus string

const (
	ContactPackageStatusPending   ContactPackageStatus = "pending"
	ContactPackageStatusActive    ContactPackageStatus = "active"
	ContactPackageStatusExhausted ContactPackageStatus = "exhausted"
	ContactPackageStatusExpired   ContactPackageStatus = "expired"
	ContactPackageStatusCancelled ContactPackageStatus = "cancelled"
	ContactPackageStatusRefunded  ContactPackageStatus = "refunded"
)

// CreditLedgerEntryType describes an immutable credit movement.
type CreditLedgerEntryType string

const (
	CreditLedgerEntryTypeGrant   CreditLedgerEntryType = "grant"
	CreditLedgerEntryTypeReserve CreditLedgerEntryType = "reserve"
	CreditLedgerEntryTypeRelease CreditLedgerEntryType = "release"
	CreditLedgerEntryTypeRedeem  CreditLedgerEntryType = "redeem"
	CreditLedgerEntryTypeExpire  CreditLedgerEntryType = "expire"
	CreditLedgerEntryTypeAdjust  CreditLedgerEntryType = "adjust"
	CreditLedgerEntryTypeReverse CreditLedgerEntryType = "reverse"
)

// CommerceInvoiceStatus is the lifecycle of a tenant invoice.
type CommerceInvoiceStatus string

const (
	CommerceInvoiceStatusDraft             CommerceInvoiceStatus = "draft"
	CommerceInvoiceStatusOpen              CommerceInvoiceStatus = "open"
	CommerceInvoiceStatusPaid              CommerceInvoiceStatus = "paid"
	CommerceInvoiceStatusVoid              CommerceInvoiceStatus = "void"
	CommerceInvoiceStatusPartiallyRefunded CommerceInvoiceStatus = "partially_refunded"
	CommerceInvoiceStatusRefunded          CommerceInvoiceStatus = "refunded"
)

// PaymentEnvironment keeps test and live provider credentials separate.
type PaymentEnvironment string

const (
	PaymentEnvironmentTest PaymentEnvironment = "test"
	PaymentEnvironmentLive PaymentEnvironment = "live"
)

// PaymentIntentStatus is the lifecycle of a provider payment intent.
type PaymentIntentStatus string

const (
	PaymentIntentStatusPending        PaymentIntentStatus = "pending"
	PaymentIntentStatusRequiresAction PaymentIntentStatus = "requires_action"
	PaymentIntentStatusSucceeded      PaymentIntentStatus = "succeeded"
	PaymentIntentStatusFailed         PaymentIntentStatus = "failed"
	PaymentIntentStatusCancelled      PaymentIntentStatus = "cancelled"
	PaymentIntentStatusExpired        PaymentIntentStatus = "expired"
)

// PaymentTransactionType describes an immutable money movement.
type PaymentTransactionType string

const (
	PaymentTransactionTypeCharge     PaymentTransactionType = "charge"
	PaymentTransactionTypeRefund     PaymentTransactionType = "refund"
	PaymentTransactionTypeFee        PaymentTransactionType = "fee"
	PaymentTransactionTypeAdjustment PaymentTransactionType = "adjustment"
)

// PaymentTransactionStatus is the normalized provider transaction status.
type PaymentTransactionStatus string

const (
	PaymentTransactionStatusPending   PaymentTransactionStatus = "pending"
	PaymentTransactionStatusSucceeded PaymentTransactionStatus = "succeeded"
	PaymentTransactionStatusFailed    PaymentTransactionStatus = "failed"
	PaymentTransactionStatusReversed  PaymentTransactionStatus = "reversed"
)

// PaymentWebhookEventStatus tracks provider webhook processing.
type PaymentWebhookEventStatus string

const (
	PaymentWebhookEventStatusReceived   PaymentWebhookEventStatus = "received"
	PaymentWebhookEventStatusProcessing PaymentWebhookEventStatus = "processing"
	PaymentWebhookEventStatusProcessed  PaymentWebhookEventStatus = "processed"
	PaymentWebhookEventStatusFailed     PaymentWebhookEventStatus = "failed"
	PaymentWebhookEventStatusIgnored    PaymentWebhookEventStatus = "ignored"
)

// CopilotTaskType identifies the requested assistant capability.
type CopilotTaskType string

const (
	CopilotTaskTypeReply          CopilotTaskType = "reply"
	CopilotTaskTypeSummary        CopilotTaskType = "summary"
	CopilotTaskTypeQualify        CopilotTaskType = "qualify"
	CopilotTaskTypeExtractActions CopilotTaskType = "extract_actions"
)

// CopilotRunStatus is the lifecycle of a Copilot request.
type CopilotRunStatus string

const (
	CopilotRunStatusPending   CopilotRunStatus = "pending"
	CopilotRunStatusRunning   CopilotRunStatus = "running"
	CopilotRunStatusCompleted CopilotRunStatus = "completed"
	CopilotRunStatusFailed    CopilotRunStatus = "failed"
	CopilotRunStatusBlocked   CopilotRunStatus = "blocked"
	CopilotRunStatusCancelled CopilotRunStatus = "cancelled"
)

// CopilotFeedbackRating is an explicit user assessment of a Copilot run.
type CopilotFeedbackRating string

const (
	CopilotFeedbackRatingHelpful    CopilotFeedbackRating = "helpful"
	CopilotFeedbackRatingNotHelpful CopilotFeedbackRating = "not_helpful"
)

// ScheduledJob is a durable delayed job claimed by background workers.
type ScheduledJob struct {
	BaseModel
	OrganizationID uuid.UUID          `gorm:"type:uuid;not null;index;uniqueIndex:idx_scheduled_jobs_org_idempotency,priority:1" json:"organization_id"`
	Kind           string             `gorm:"size:80;not null;index" json:"kind"`
	AggregateType  string             `gorm:"size:80;index" json:"aggregate_type,omitempty"`
	AggregateID    *uuid.UUID         `gorm:"type:uuid;index" json:"aggregate_id,omitempty"`
	RunAt          time.Time          `gorm:"not null;index:idx_scheduled_jobs_due" json:"run_at"`
	Status         ScheduledJobStatus `gorm:"size:20;not null;default:'pending';index:idx_scheduled_jobs_due" json:"status"`
	Attempts       int                `gorm:"not null;default:0" json:"attempts"`
	MaxAttempts    int                `gorm:"not null;default:5" json:"max_attempts"`
	IdempotencyKey string             `gorm:"size:255;not null;uniqueIndex:idx_scheduled_jobs_org_idempotency,priority:2" json:"idempotency_key"`
	Payload        JSONB              `gorm:"type:jsonb;default:'{}'" json:"payload"`
	LockedAt       *time.Time         `gorm:"index" json:"locked_at,omitempty"`
	LockedBy       string             `gorm:"size:255" json:"locked_by,omitempty"`
	CompletedAt    *time.Time         `json:"completed_at,omitempty"`
	LastError      string             `gorm:"type:text" json:"last_error,omitempty"`
	Version        int64              `gorm:"not null;default:1" json:"version"`
}

func (ScheduledJob) TableName() string {
	return "scheduled_jobs"
}

// OutboxEvent is written atomically with a domain change and delivered later.
type OutboxEvent struct {
	BaseModel
	OrganizationID uuid.UUID         `gorm:"type:uuid;not null;index;uniqueIndex:idx_outbox_events_org_idempotency,priority:1" json:"organization_id"`
	EventType      string            `gorm:"size:120;not null;index" json:"event_type"`
	AggregateType  string            `gorm:"size:80;index" json:"aggregate_type,omitempty"`
	AggregateID    *uuid.UUID        `gorm:"type:uuid;index" json:"aggregate_id,omitempty"`
	Payload        JSONB             `gorm:"type:jsonb;default:'{}'" json:"payload"`
	AvailableAt    time.Time         `gorm:"not null;index:idx_outbox_events_due" json:"available_at"`
	Status         OutboxEventStatus `gorm:"size:20;not null;default:'pending';index:idx_outbox_events_due" json:"status"`
	Attempts       int               `gorm:"not null;default:0" json:"attempts"`
	MaxAttempts    int               `gorm:"not null;default:10" json:"max_attempts"`
	IdempotencyKey string            `gorm:"size:255;not null;uniqueIndex:idx_outbox_events_org_idempotency,priority:2" json:"idempotency_key"`
	LockedAt       *time.Time        `gorm:"index" json:"locked_at,omitempty"`
	LockedBy       string            `gorm:"size:255" json:"locked_by,omitempty"`
	PublishedAt    *time.Time        `json:"published_at,omitempty"`
	LastError      string            `gorm:"type:text" json:"last_error,omitempty"`
	Version        int64             `gorm:"not null;default:1" json:"version"`
}

func (OutboxEvent) TableName() string {
	return "outbox_events"
}

// CustomerActivityEvent is the append-only, customer-facing lifecycle stream.
// It deliberately does not embed BaseModel: an event can be corrected only by
// appending another event, never by updating or soft-deleting history.
type CustomerActivityEvent struct {
	ID               uuid.UUID                 `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	OrganizationID   uuid.UUID                 `gorm:"type:uuid;not null;index;uniqueIndex:idx_customer_activity_org_idempotency,priority:1" json:"organization_id"`
	ContactID        uuid.UUID                 `gorm:"type:uuid;not null;index" json:"contact_id"`
	LeadID           *uuid.UUID                `gorm:"type:uuid;index" json:"lead_id,omitempty"`
	EventType        CustomerActivityEventType `gorm:"size:120;not null;index" json:"event_type"`
	Category         CustomerActivityCategory  `gorm:"size:30;not null;index" json:"category"`
	Title            string                    `gorm:"size:255;not null" json:"title"`
	Summary          string                    `gorm:"type:text" json:"summary,omitempty"`
	ActorType        CustomerActivityActorType `gorm:"size:30;not null;default:'system';index" json:"actor_type"`
	ActorUserID      *uuid.UUID                `gorm:"type:uuid;index" json:"actor_user_id,omitempty"`
	SourceObjectType string                    `gorm:"size:80;not null;index" json:"source_object_type"`
	SourceObjectID   *uuid.UUID                `gorm:"type:uuid;index" json:"source_object_id,omitempty"`
	OccurredAt       time.Time                 `gorm:"not null;index" json:"occurred_at"`
	Metadata         JSONB                     `gorm:"type:jsonb;not null;default:'{}'" json:"metadata"`
	IdempotencyKey   string                    `gorm:"size:255;not null;uniqueIndex:idx_customer_activity_org_idempotency,priority:2" json:"idempotency_key"`
	CreatedAt        time.Time                 `gorm:"not null;autoCreateTime" json:"created_at"`

	Contact *Contact `gorm:"foreignKey:ContactID" json:"contact,omitempty"`
	Lead    *CRMLead `gorm:"foreignKey:LeadID" json:"lead,omitempty"`
	Actor   *User    `gorm:"foreignKey:ActorUserID" json:"actor,omitempty"`
}

func (CustomerActivityEvent) TableName() string {
	return "customer_activity_events"
}

// CRMPipeline is an organization-defined lead lifecycle.
type CRMPipeline struct {
	BaseModel
	OrganizationID uuid.UUID  `gorm:"type:uuid;not null;index" json:"organization_id"`
	Name           string     `gorm:"size:150;not null" json:"name"`
	Description    string     `gorm:"type:text" json:"description,omitempty"`
	IsDefault      bool       `gorm:"not null;default:false;index" json:"is_default"`
	IsActive       bool       `gorm:"not null;default:true;index" json:"is_active"`
	DisplayOrder   int        `gorm:"not null;default:0" json:"display_order"`
	Version        int64      `gorm:"not null;default:1" json:"version"`
	CreatedByID    *uuid.UUID `gorm:"type:uuid" json:"created_by_id,omitempty"`
	UpdatedByID    *uuid.UUID `gorm:"type:uuid" json:"updated_by_id,omitempty"`

	Stages []CRMPipelineStage `gorm:"foreignKey:PipelineID" json:"stages,omitempty"`
}

func (CRMPipeline) TableName() string {
	return "crm_pipelines"
}

// CRMPipelineStage is an ordered stage within a CRM pipeline.
type CRMPipelineStage struct {
	BaseModel
	OrganizationID uuid.UUID            `gorm:"type:uuid;not null;index" json:"organization_id"`
	PipelineID     uuid.UUID            `gorm:"type:uuid;not null;index" json:"pipeline_id"`
	Name           string               `gorm:"size:150;not null" json:"name"`
	Color          string               `gorm:"size:20" json:"color,omitempty"`
	DisplayOrder   int                  `gorm:"not null;default:0;index" json:"display_order"`
	Kind           CRMPipelineStageKind `gorm:"size:20;not null;default:'open';index" json:"kind"`
	Probability    int                  `gorm:"not null;default:0" json:"probability"`
	SLAHours       int                  `gorm:"not null;default:0" json:"sla_hours"`
	IsActive       bool                 `gorm:"not null;default:true;index" json:"is_active"`
	Version        int64                `gorm:"not null;default:1" json:"version"`
	CreatedByID    *uuid.UUID           `gorm:"type:uuid" json:"created_by_id,omitempty"`
	UpdatedByID    *uuid.UUID           `gorm:"type:uuid" json:"updated_by_id,omitempty"`

	Pipeline *CRMPipeline `gorm:"foreignKey:PipelineID" json:"pipeline,omitempty"`
}

func (CRMPipelineStage) TableName() string {
	return "crm_pipeline_stages"
}

// CRMLead is an organization-scoped sales or service opportunity.
type CRMLead struct {
	BaseModel
	OrganizationID     uuid.UUID     `gorm:"type:uuid;not null;index" json:"organization_id"`
	ContactID          uuid.UUID     `gorm:"type:uuid;not null;index" json:"contact_id"`
	PipelineID         uuid.UUID     `gorm:"type:uuid;not null;index" json:"pipeline_id"`
	StageID            uuid.UUID     `gorm:"type:uuid;not null;index" json:"stage_id"`
	Title              string        `gorm:"size:255;not null" json:"title"`
	Status             CRMLeadStatus `gorm:"size:20;not null;default:'open';index" json:"status"`
	OwnerUserID        *uuid.UUID    `gorm:"type:uuid;index" json:"owner_user_id,omitempty"`
	Source             CRMLeadSource `gorm:"size:30;not null;default:'other';index" json:"source"`
	SourceReference    string        `gorm:"size:255;index" json:"source_reference,omitempty"`
	ValueMinor         int64         `gorm:"not null;default:0" json:"value_minor"`
	Currency           string        `gorm:"size:3;not null;default:'MYR'" json:"currency"`
	NextActionAt       *time.Time    `gorm:"index" json:"next_action_at,omitempty"`
	ExpectedCloseDate  *time.Time    `json:"expected_close_date,omitempty"`
	LastActivityAt     *time.Time    `gorm:"index" json:"last_activity_at,omitempty"`
	WonAt              *time.Time    `json:"won_at,omitempty"`
	LostAt             *time.Time    `json:"lost_at,omitempty"`
	LostReason         string        `gorm:"type:text" json:"lost_reason,omitempty"`
	IdempotencyKey     string        `gorm:"size:255;index" json:"idempotency_key,omitempty"`
	RequestFingerprint string        `gorm:"size:64" json:"-"`
	Metadata           JSONB         `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	Version            int64         `gorm:"not null;default:1" json:"version"`
	CreatedByID        *uuid.UUID    `gorm:"type:uuid" json:"created_by_id,omitempty"`
	UpdatedByID        *uuid.UUID    `gorm:"type:uuid" json:"updated_by_id,omitempty"`

	Contact  *Contact          `gorm:"foreignKey:ContactID" json:"contact,omitempty"`
	Pipeline *CRMPipeline      `gorm:"foreignKey:PipelineID" json:"pipeline,omitempty"`
	Stage    *CRMPipelineStage `gorm:"foreignKey:StageID" json:"stage,omitempty"`
	Owner    *User             `gorm:"foreignKey:OwnerUserID" json:"owner,omitempty"`
	History  []CRMStageHistory `gorm:"foreignKey:LeadID" json:"history,omitempty"`
}

func (CRMLead) TableName() string {
	return "crm_leads"
}

// CRMStageHistory is an append-only record of a lead stage transition.
type CRMStageHistory struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	OrganizationID uuid.UUID  `gorm:"type:uuid;not null;index" json:"organization_id"`
	LeadID         uuid.UUID  `gorm:"type:uuid;not null;index" json:"lead_id"`
	FromStageID    *uuid.UUID `gorm:"type:uuid;index" json:"from_stage_id,omitempty"`
	ToStageID      uuid.UUID  `gorm:"type:uuid;not null;index" json:"to_stage_id"`
	ChangedByID    *uuid.UUID `gorm:"type:uuid;index" json:"changed_by_id,omitempty"`
	Reason         string     `gorm:"type:text" json:"reason,omitempty"`
	Metadata       JSONB      `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	ChangedAt      time.Time  `gorm:"not null;autoCreateTime;index" json:"changed_at"`

	Lead      *CRMLead          `gorm:"foreignKey:LeadID" json:"lead,omitempty"`
	FromStage *CRMPipelineStage `gorm:"foreignKey:FromStageID" json:"from_stage,omitempty"`
	ToStage   *CRMPipelineStage `gorm:"foreignKey:ToStageID" json:"to_stage,omitempty"`
	ChangedBy *User             `gorm:"foreignKey:ChangedByID" json:"changed_by,omitempty"`
}

func (CRMStageHistory) TableName() string {
	return "crm_stage_history"
}

// FollowUpTask is a user-owned action linked to a contact or domain record.
type FollowUpTask struct {
	BaseModel
	OrganizationID     uuid.UUID            `gorm:"type:uuid;not null;index" json:"organization_id"`
	ContactID          *uuid.UUID           `gorm:"type:uuid;index" json:"contact_id,omitempty"`
	LeadID             *uuid.UUID           `gorm:"type:uuid;index" json:"lead_id,omitempty"`
	BookingID          *uuid.UUID           `gorm:"type:uuid;index" json:"booking_id,omitempty"`
	Title              string               `gorm:"size:255;not null" json:"title"`
	Description        string               `gorm:"type:text" json:"description,omitempty"`
	Status             FollowUpTaskStatus   `gorm:"size:20;not null;default:'open';index" json:"status"`
	Priority           FollowUpTaskPriority `gorm:"size:20;not null;default:'normal';index" json:"priority"`
	OwnerUserID        *uuid.UUID           `gorm:"type:uuid;index" json:"owner_user_id,omitempty"`
	DueAt              *time.Time           `gorm:"index" json:"due_at,omitempty"`
	RemindAt           *time.Time           `gorm:"index" json:"remind_at,omitempty"`
	CompletedAt        *time.Time           `json:"completed_at,omitempty"`
	CompletedByID      *uuid.UUID           `gorm:"type:uuid" json:"completed_by_id,omitempty"`
	RecurrenceRule     string               `gorm:"size:500" json:"recurrence_rule,omitempty"`
	ParentTaskID       *uuid.UUID           `gorm:"type:uuid;index" json:"parent_task_id,omitempty"`
	Source             string               `gorm:"size:50;index" json:"source,omitempty"`
	IdempotencyKey     string               `gorm:"size:255;index" json:"idempotency_key,omitempty"`
	RequestFingerprint string               `gorm:"size:64" json:"-"`
	Metadata           JSONB                `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	Version            int64                `gorm:"not null;default:1" json:"version"`
	CreatedByID        *uuid.UUID           `gorm:"type:uuid" json:"created_by_id,omitempty"`
	UpdatedByID        *uuid.UUID           `gorm:"type:uuid" json:"updated_by_id,omitempty"`

	Contact     *Contact      `gorm:"foreignKey:ContactID" json:"contact,omitempty"`
	Lead        *CRMLead      `gorm:"foreignKey:LeadID" json:"lead,omitempty"`
	Booking     *Booking      `gorm:"foreignKey:BookingID" json:"booking,omitempty"`
	Owner       *User         `gorm:"foreignKey:OwnerUserID" json:"owner,omitempty"`
	CompletedBy *User         `gorm:"foreignKey:CompletedByID" json:"completed_by,omitempty"`
	ParentTask  *FollowUpTask `gorm:"foreignKey:ParentTaskID" json:"parent_task,omitempty"`
}

func (FollowUpTask) TableName() string {
	return "follow_up_tasks"
}

// BookingService is a tenant-defined appointment or class offering.
type BookingService struct {
	BaseModel
	OrganizationID   uuid.UUID          `gorm:"type:uuid;not null;index" json:"organization_id"`
	Name             string             `gorm:"size:255;not null" json:"name"`
	Description      string             `gorm:"type:text" json:"description,omitempty"`
	Kind             BookingServiceKind `gorm:"size:20;not null;default:'appointment';index" json:"kind"`
	DurationMinutes  int                `gorm:"not null;default:30" json:"duration_minutes"`
	BufferBeforeMins int                `gorm:"not null;default:0" json:"buffer_before_minutes"`
	BufferAfterMins  int                `gorm:"not null;default:0" json:"buffer_after_minutes"`
	DefaultCapacity  int                `gorm:"not null;default:1" json:"default_capacity"`
	PriceMinor       int64              `gorm:"not null;default:0" json:"price_minor"`
	Currency         string             `gorm:"size:3;not null;default:'MYR'" json:"currency"`
	ReminderPolicy   JSONB              `gorm:"type:jsonb;default:'{}'" json:"reminder_policy"`
	IsActive         bool               `gorm:"not null;default:true;index" json:"is_active"`
	Metadata         JSONB              `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	Version          int64              `gorm:"not null;default:1" json:"version"`
	CreatedByID      *uuid.UUID         `gorm:"type:uuid" json:"created_by_id,omitempty"`
	UpdatedByID      *uuid.UUID         `gorm:"type:uuid" json:"updated_by_id,omitempty"`
}

func (BookingService) TableName() string {
	return "booking_services"
}

// BookingResource is a practitioner, instructor, room, or piece of equipment.
type BookingResource struct {
	BaseModel
	OrganizationID uuid.UUID           `gorm:"type:uuid;not null;index" json:"organization_id"`
	UserID         *uuid.UUID          `gorm:"type:uuid;index" json:"user_id,omitempty"`
	Name           string              `gorm:"size:255;not null" json:"name"`
	Kind           BookingResourceKind `gorm:"size:30;not null;index" json:"kind"`
	Timezone       string              `gorm:"size:100;not null;default:'UTC'" json:"timezone"`
	Location       string              `gorm:"size:255" json:"location,omitempty"`
	IsActive       bool                `gorm:"not null;default:true;index" json:"is_active"`
	Metadata       JSONB               `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	Version        int64               `gorm:"not null;default:1" json:"version"`
	CreatedByID    *uuid.UUID          `gorm:"type:uuid" json:"created_by_id,omitempty"`
	UpdatedByID    *uuid.UUID          `gorm:"type:uuid" json:"updated_by_id,omitempty"`

	User *User `gorm:"foreignKey:UserID" json:"user,omitempty"`
}

func (BookingResource) TableName() string {
	return "booking_resources"
}

// BookingServiceResource links an offering to an eligible resource.
type BookingServiceResource struct {
	BaseModel
	OrganizationID uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:idx_booking_service_resource_org,priority:1" json:"organization_id"`
	ServiceID      uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:idx_booking_service_resource_org,priority:2" json:"service_id"`
	ResourceID     uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:idx_booking_service_resource_org,priority:3" json:"resource_id"`
	IsActive       bool      `gorm:"not null;default:true;index" json:"is_active"`
	Version        int64     `gorm:"not null;default:1" json:"version"`

	Service  *BookingService  `gorm:"foreignKey:ServiceID" json:"service,omitempty"`
	Resource *BookingResource `gorm:"foreignKey:ResourceID" json:"resource,omitempty"`
}

func (BookingServiceResource) TableName() string {
	return "booking_service_resources"
}

// AvailabilityRule stores a resource's recurring local-time availability.
type AvailabilityRule struct {
	BaseModel
	OrganizationID uuid.UUID  `gorm:"type:uuid;not null;index" json:"organization_id"`
	ResourceID     uuid.UUID  `gorm:"type:uuid;not null;index" json:"resource_id"`
	Weekday        int        `gorm:"not null;index" json:"weekday"`
	StartLocalTime string     `gorm:"size:5;not null" json:"start_local_time"`
	EndLocalTime   string     `gorm:"size:5;not null" json:"end_local_time"`
	EffectiveFrom  *time.Time `json:"effective_from,omitempty"`
	EffectiveUntil *time.Time `json:"effective_until,omitempty"`
	IsActive       bool       `gorm:"not null;default:true;index" json:"is_active"`
	Version        int64      `gorm:"not null;default:1" json:"version"`
	CreatedByID    *uuid.UUID `gorm:"type:uuid" json:"created_by_id,omitempty"`
	UpdatedByID    *uuid.UUID `gorm:"type:uuid" json:"updated_by_id,omitempty"`

	Resource *BookingResource `gorm:"foreignKey:ResourceID" json:"resource,omitempty"`
}

func (AvailabilityRule) TableName() string {
	return "availability_rules"
}

// ResourceTimeOff blocks a resource for an absolute UTC interval.
type ResourceTimeOff struct {
	BaseModel
	OrganizationID uuid.UUID  `gorm:"type:uuid;not null;index" json:"organization_id"`
	ResourceID     uuid.UUID  `gorm:"type:uuid;not null;index" json:"resource_id"`
	StartsAt       time.Time  `gorm:"not null;index" json:"starts_at"`
	EndsAt         time.Time  `gorm:"not null;index" json:"ends_at"`
	Reason         string     `gorm:"type:text" json:"reason,omitempty"`
	Version        int64      `gorm:"not null;default:1" json:"version"`
	CreatedByID    *uuid.UUID `gorm:"type:uuid" json:"created_by_id,omitempty"`
	UpdatedByID    *uuid.UUID `gorm:"type:uuid" json:"updated_by_id,omitempty"`

	Resource *BookingResource `gorm:"foreignKey:ResourceID" json:"resource,omitempty"`
}

func (ResourceTimeOff) TableName() string {
	return "resource_time_off"
}

// BookingEvent is the calendar slot shared by one or more contact bookings.
type BookingEvent struct {
	BaseModel
	OrganizationID   uuid.UUID          `gorm:"type:uuid;not null;index" json:"organization_id"`
	ServiceID        uuid.UUID          `gorm:"type:uuid;not null;index" json:"service_id"`
	ResourceID       uuid.UUID          `gorm:"type:uuid;not null;index" json:"resource_id"`
	StartsAt         time.Time          `gorm:"not null;index:idx_booking_events_resource_time" json:"starts_at"`
	EndsAt           time.Time          `gorm:"not null;index:idx_booking_events_resource_time" json:"ends_at"`
	Capacity         int                `gorm:"not null;default:1" json:"capacity"`
	Status           BookingEventStatus `gorm:"size:20;not null;default:'scheduled';index" json:"status"`
	RecurrenceID     *uuid.UUID         `gorm:"type:uuid;index" json:"recurrence_id,omitempty"`
	ExternalProvider string             `gorm:"size:50;index" json:"external_provider,omitempty"`
	ExternalEventID  string             `gorm:"size:255;index" json:"external_event_id,omitempty"`
	Location         string             `gorm:"size:255" json:"location,omitempty"`
	Metadata         JSONB              `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	Version          int64              `gorm:"not null;default:1" json:"version"`
	CreatedByID      *uuid.UUID         `gorm:"type:uuid" json:"created_by_id,omitempty"`
	UpdatedByID      *uuid.UUID         `gorm:"type:uuid" json:"updated_by_id,omitempty"`

	Service  *BookingService  `gorm:"foreignKey:ServiceID" json:"service,omitempty"`
	Resource *BookingResource `gorm:"foreignKey:ResourceID" json:"resource,omitempty"`
	Bookings []Booking        `gorm:"foreignKey:EventID" json:"bookings,omitempty"`
}

func (BookingEvent) TableName() string {
	return "booking_events"
}

// Booking is a contact's attendance record for a booking event.
type Booking struct {
	BaseModel
	OrganizationID     uuid.UUID     `gorm:"type:uuid;not null;index;uniqueIndex:idx_bookings_org_idempotency,priority:1" json:"organization_id"`
	EventID            uuid.UUID     `gorm:"type:uuid;not null;index" json:"event_id"`
	ContactID          uuid.UUID     `gorm:"type:uuid;not null;index" json:"contact_id"`
	Status             BookingStatus `gorm:"size:20;not null;default:'reserved';index" json:"status"`
	Quantity           int           `gorm:"not null;default:1" json:"quantity"`
	Source             BookingSource `gorm:"size:30;not null;default:'agent';index" json:"source"`
	Notes              string        `gorm:"type:text" json:"notes,omitempty"`
	ContactPackageID   *uuid.UUID    `gorm:"type:uuid;index" json:"contact_package_id,omitempty"`
	BookedByID         *uuid.UUID    `gorm:"type:uuid;index" json:"booked_by_id,omitempty"`
	ConfirmedAt        *time.Time    `json:"confirmed_at,omitempty"`
	CheckedInAt        *time.Time    `json:"checked_in_at,omitempty"`
	CompletedAt        *time.Time    `json:"completed_at,omitempty"`
	NoShowAt           *time.Time    `json:"no_show_at,omitempty"`
	CancelledAt        *time.Time    `json:"cancelled_at,omitempty"`
	CancelledByID      *uuid.UUID    `gorm:"type:uuid" json:"cancelled_by_id,omitempty"`
	CancellationReason string        `gorm:"type:text" json:"cancellation_reason,omitempty"`
	IdempotencyKey     string        `gorm:"size:255;not null;uniqueIndex:idx_bookings_org_idempotency,priority:2" json:"idempotency_key"`
	Metadata           JSONB         `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	Version            int64         `gorm:"not null;default:1" json:"version"`
	UpdatedByID        *uuid.UUID    `gorm:"type:uuid" json:"updated_by_id,omitempty"`

	Event          *BookingEvent   `gorm:"foreignKey:EventID" json:"event,omitempty"`
	Contact        *Contact        `gorm:"foreignKey:ContactID" json:"contact,omitempty"`
	ContactPackage *ContactPackage `gorm:"foreignKey:ContactPackageID" json:"contact_package,omitempty"`
	BookedBy       *User           `gorm:"foreignKey:BookedByID" json:"booked_by,omitempty"`
	CancelledBy    *User           `gorm:"foreignKey:CancelledByID" json:"cancelled_by,omitempty"`
}

func (Booking) TableName() string {
	return "bookings"
}

// PackageDefinition is a sellable bundle of service entitlements.
type PackageDefinition struct {
	BaseModel
	OrganizationID uuid.UUID  `gorm:"type:uuid;not null;index" json:"organization_id"`
	Name           string     `gorm:"size:255;not null" json:"name"`
	Description    string     `gorm:"type:text" json:"description,omitempty"`
	PriceMinor     int64      `gorm:"not null;default:0" json:"price_minor"`
	Currency       string     `gorm:"size:3;not null;default:'MYR'" json:"currency"`
	ValidityDays   int        `gorm:"not null;default:0" json:"validity_days"`
	IsActive       bool       `gorm:"not null;default:true;index" json:"is_active"`
	Metadata       JSONB      `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	Version        int64      `gorm:"not null;default:1" json:"version"`
	CreatedByID    *uuid.UUID `gorm:"type:uuid" json:"created_by_id,omitempty"`
	UpdatedByID    *uuid.UUID `gorm:"type:uuid" json:"updated_by_id,omitempty"`

	Entitlements []PackageEntitlement `gorm:"foreignKey:PackageDefinitionID" json:"entitlements,omitempty"`
}

func (PackageDefinition) TableName() string {
	return "package_definitions"
}

// PackageEntitlement grants credits for a booking service.
type PackageEntitlement struct {
	BaseModel
	OrganizationID      uuid.UUID `gorm:"type:uuid;not null;index" json:"organization_id"`
	PackageDefinitionID uuid.UUID `gorm:"type:uuid;not null;index" json:"package_definition_id"`
	BookingServiceID    uuid.UUID `gorm:"type:uuid;not null;index" json:"booking_service_id"`
	Credits             int       `gorm:"not null;default:0" json:"credits"`
	IsUnlimited         bool      `gorm:"not null;default:false" json:"is_unlimited"`
	Version             int64     `gorm:"not null;default:1" json:"version"`

	PackageDefinition *PackageDefinition `gorm:"foreignKey:PackageDefinitionID" json:"package_definition,omitempty"`
	BookingService    *BookingService    `gorm:"foreignKey:BookingServiceID" json:"booking_service,omitempty"`
}

func (PackageEntitlement) TableName() string {
	return "package_entitlements"
}

// ContactPackage is a purchased package owned by one contact.
type ContactPackage struct {
	BaseModel
	OrganizationID      uuid.UUID            `gorm:"type:uuid;not null;index;uniqueIndex:idx_contact_packages_org_idempotency,priority:1" json:"organization_id"`
	ContactID           uuid.UUID            `gorm:"type:uuid;not null;index" json:"contact_id"`
	PackageDefinitionID uuid.UUID            `gorm:"type:uuid;not null;index" json:"package_definition_id"`
	InvoiceID           *uuid.UUID           `gorm:"type:uuid;index" json:"invoice_id,omitempty"`
	Status              ContactPackageStatus `gorm:"size:20;not null;default:'pending';index" json:"status"`
	StartsAt            *time.Time           `json:"starts_at,omitempty"`
	ExpiresAt           *time.Time           `gorm:"index" json:"expires_at,omitempty"`
	PurchaseAmountMinor int64                `gorm:"not null;default:0" json:"purchase_amount_minor"`
	Currency            string               `gorm:"size:3;not null;default:'MYR'" json:"currency"`
	Source              string               `gorm:"size:50;index" json:"source,omitempty"`
	IdempotencyKey      string               `gorm:"size:255;not null;uniqueIndex:idx_contact_packages_org_idempotency,priority:2" json:"idempotency_key"`
	Metadata            JSONB                `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	Version             int64                `gorm:"not null;default:1" json:"version"`
	CreatedByID         *uuid.UUID           `gorm:"type:uuid" json:"created_by_id,omitempty"`
	UpdatedByID         *uuid.UUID           `gorm:"type:uuid" json:"updated_by_id,omitempty"`

	Contact           *Contact           `gorm:"foreignKey:ContactID" json:"contact,omitempty"`
	PackageDefinition *PackageDefinition `gorm:"foreignKey:PackageDefinitionID" json:"package_definition,omitempty"`
	Invoice           *CommerceInvoice   `gorm:"foreignKey:InvoiceID" json:"invoice,omitempty"`
	Balances          []CreditBalance    `gorm:"foreignKey:ContactPackageID" json:"balances,omitempty"`
}

func (ContactPackage) TableName() string {
	return "contact_packages"
}

// CreditBalance is the lockable materialized balance for one entitlement.
type CreditBalance struct {
	BaseModel
	OrganizationID       uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:idx_credit_balance_entitlement,priority:1" json:"organization_id"`
	ContactPackageID     uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:idx_credit_balance_entitlement,priority:2" json:"contact_package_id"`
	PackageEntitlementID uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:idx_credit_balance_entitlement,priority:3" json:"package_entitlement_id"`
	Granted              int       `gorm:"not null;default:0" json:"granted"`
	Reserved             int       `gorm:"not null;default:0" json:"reserved"`
	Consumed             int       `gorm:"not null;default:0" json:"consumed"`
	Available            int       `gorm:"not null;default:0" json:"available"`
	Version              int64     `gorm:"not null;default:1" json:"version"`

	ContactPackage     *ContactPackage     `gorm:"foreignKey:ContactPackageID" json:"contact_package,omitempty"`
	PackageEntitlement *PackageEntitlement `gorm:"foreignKey:PackageEntitlementID" json:"package_entitlement,omitempty"`
}

func (CreditBalance) TableName() string {
	return "credit_balances"
}

// CreditLedgerEntry is append-only. It deliberately does not embed BaseModel.
type CreditLedgerEntry struct {
	ID                   uuid.UUID             `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	OrganizationID       uuid.UUID             `gorm:"type:uuid;not null;index;uniqueIndex:idx_credit_ledger_org_idempotency,priority:1" json:"organization_id"`
	ContactPackageID     uuid.UUID             `gorm:"type:uuid;not null;index" json:"contact_package_id"`
	PackageEntitlementID uuid.UUID             `gorm:"type:uuid;not null;index" json:"package_entitlement_id"`
	BookingID            *uuid.UUID            `gorm:"type:uuid;index" json:"booking_id,omitempty"`
	PaymentTransactionID *uuid.UUID            `gorm:"type:uuid;index" json:"payment_transaction_id,omitempty"`
	Type                 CreditLedgerEntryType `gorm:"size:20;not null;index" json:"type"`
	Delta                int                   `gorm:"not null" json:"delta"`
	BalanceAfter         int                   `gorm:"not null" json:"balance_after"`
	IdempotencyKey       string                `gorm:"size:255;not null;uniqueIndex:idx_credit_ledger_org_idempotency,priority:2" json:"idempotency_key"`
	ReversalOfID         *uuid.UUID            `gorm:"type:uuid;index" json:"reversal_of_id,omitempty"`
	Reason               string                `gorm:"type:text" json:"reason,omitempty"`
	ActorUserID          *uuid.UUID            `gorm:"type:uuid;index" json:"actor_user_id,omitempty"`
	Metadata             JSONB                 `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	OccurredAt           time.Time             `gorm:"not null;index" json:"occurred_at"`
	CreatedAt            time.Time             `gorm:"not null;autoCreateTime" json:"created_at"`

	ContactPackage     *ContactPackage     `gorm:"foreignKey:ContactPackageID" json:"contact_package,omitempty"`
	PackageEntitlement *PackageEntitlement `gorm:"foreignKey:PackageEntitlementID" json:"package_entitlement,omitempty"`
	Booking            *Booking            `gorm:"foreignKey:BookingID" json:"booking,omitempty"`
	PaymentTransaction *PaymentTransaction `gorm:"foreignKey:PaymentTransactionID" json:"payment_transaction,omitempty"`
	ReversalOf         *CreditLedgerEntry  `gorm:"foreignKey:ReversalOfID" json:"reversal_of,omitempty"`
	Actor              *User               `gorm:"foreignKey:ActorUserID" json:"actor,omitempty"`
}

func (CreditLedgerEntry) TableName() string {
	return "credit_ledger_entries"
}

// CommerceInvoice is a tenant invoice for bookings, packages, or custom lines.
type CommerceInvoice struct {
	BaseModel
	OrganizationID uuid.UUID             `gorm:"type:uuid;not null;index;uniqueIndex:idx_commerce_invoices_org_number,priority:1;uniqueIndex:idx_commerce_invoices_org_idempotency,priority:1" json:"organization_id"`
	ContactID      uuid.UUID             `gorm:"type:uuid;not null;index" json:"contact_id"`
	InvoiceNumber  string                `gorm:"size:100;not null;index;uniqueIndex:idx_commerce_invoices_org_number,priority:2" json:"invoice_number"`
	IdempotencyKey string                `gorm:"size:255;not null;uniqueIndex:idx_commerce_invoices_org_idempotency,priority:2" json:"idempotency_key"`
	Status         CommerceInvoiceStatus `gorm:"size:30;not null;default:'draft';index" json:"status"`
	Currency       string                `gorm:"size:3;not null;default:'MYR'" json:"currency"`
	SubtotalMinor  int64                 `gorm:"not null;default:0" json:"subtotal_minor"`
	DiscountMinor  int64                 `gorm:"not null;default:0" json:"discount_minor"`
	TaxMinor       int64                 `gorm:"not null;default:0" json:"tax_minor"`
	TotalMinor     int64                 `gorm:"not null;default:0" json:"total_minor"`
	PaidMinor      int64                 `gorm:"not null;default:0" json:"paid_minor"`
	DueMinor       int64                 `gorm:"not null;default:0" json:"due_minor"`
	IssuedAt       *time.Time            `json:"issued_at,omitempty"`
	DueAt          *time.Time            `gorm:"index" json:"due_at,omitempty"`
	PaidAt         *time.Time            `json:"paid_at,omitempty"`
	Metadata       JSONB                 `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	Version        int64                 `gorm:"not null;default:1" json:"version"`
	CreatedByID    *uuid.UUID            `gorm:"type:uuid" json:"created_by_id,omitempty"`
	UpdatedByID    *uuid.UUID            `gorm:"type:uuid" json:"updated_by_id,omitempty"`

	Contact *Contact      `gorm:"foreignKey:ContactID" json:"contact,omitempty"`
	Lines   []InvoiceLine `gorm:"foreignKey:InvoiceID" json:"lines,omitempty"`
}

func (CommerceInvoice) TableName() string {
	return "commerce_invoices"
}

// InvoiceLine is an amount-bearing row on a CommerceInvoice.
type InvoiceLine struct {
	BaseModel
	OrganizationID      uuid.UUID  `gorm:"type:uuid;not null;index" json:"organization_id"`
	InvoiceID           uuid.UUID  `gorm:"type:uuid;not null;index" json:"invoice_id"`
	BookingID           *uuid.UUID `gorm:"type:uuid;index" json:"booking_id,omitempty"`
	ContactPackageID    *uuid.UUID `gorm:"type:uuid;index" json:"contact_package_id,omitempty"`
	PackageDefinitionID *uuid.UUID `gorm:"type:uuid;index" json:"package_definition_id,omitempty"`
	Description         string     `gorm:"type:text;not null" json:"description"`
	Quantity            int        `gorm:"not null;default:1" json:"quantity"`
	UnitAmountMinor     int64      `gorm:"not null;default:0" json:"unit_amount_minor"`
	SubtotalMinor       int64      `gorm:"not null;default:0" json:"subtotal_minor"`
	TaxMinor            int64      `gorm:"not null;default:0" json:"tax_minor"`
	TotalMinor          int64      `gorm:"not null;default:0" json:"total_minor"`
	Metadata            JSONB      `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	Version             int64      `gorm:"not null;default:1" json:"version"`

	Invoice           *CommerceInvoice   `gorm:"foreignKey:InvoiceID" json:"invoice,omitempty"`
	Booking           *Booking           `gorm:"foreignKey:BookingID" json:"booking,omitempty"`
	ContactPackage    *ContactPackage    `gorm:"foreignKey:ContactPackageID" json:"contact_package,omitempty"`
	PackageDefinition *PackageDefinition `gorm:"foreignKey:PackageDefinitionID" json:"package_definition,omitempty"`
}

func (InvoiceLine) TableName() string {
	return "invoice_lines"
}

// PaymentProviderAccount holds tenant-specific payment provider credentials.
type PaymentProviderAccount struct {
	BaseModel
	OrganizationID         uuid.UUID          `gorm:"type:uuid;not null;index" json:"organization_id"`
	Name                   string             `gorm:"size:150;not null" json:"name"`
	Provider               string             `gorm:"size:50;not null;index" json:"provider"`
	ExternalAccountID      string             `gorm:"size:255;not null;index" json:"external_account_id"`
	Environment            PaymentEnvironment `gorm:"size:10;not null;default:'test';index" json:"environment"`
	APIKeyEncrypted        string             `gorm:"type:text" json:"-"`
	APISecretEncrypted     string             `gorm:"type:text" json:"-"`
	WebhookSecretEncrypted string             `gorm:"type:text" json:"-"`
	PublicConfig           JSONB              `gorm:"type:jsonb;default:'{}'" json:"public_config"`
	IsActive               bool               `gorm:"not null;default:true;index" json:"is_active"`
	Metadata               JSONB              `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	Version                int64              `gorm:"not null;default:1" json:"version"`
	CreatedByID            *uuid.UUID         `gorm:"type:uuid" json:"created_by_id,omitempty"`
	UpdatedByID            *uuid.UUID         `gorm:"type:uuid" json:"updated_by_id,omitempty"`
}

func (PaymentProviderAccount) TableName() string {
	return "payment_provider_accounts"
}

// PaymentIntent represents a request to collect money for an invoice.
type PaymentIntent struct {
	BaseModel
	OrganizationID    uuid.UUID           `gorm:"type:uuid;not null;index;uniqueIndex:idx_payment_intents_org_idempotency,priority:1" json:"organization_id"`
	ProviderAccountID uuid.UUID           `gorm:"type:uuid;not null;index" json:"provider_account_id"`
	InvoiceID         uuid.UUID           `gorm:"type:uuid;not null;index" json:"invoice_id"`
	ContactID         uuid.UUID           `gorm:"type:uuid;not null;index" json:"contact_id"`
	ProviderIntentID  string              `gorm:"size:255;index" json:"provider_intent_id,omitempty"`
	IdempotencyKey    string              `gorm:"size:255;not null;uniqueIndex:idx_payment_intents_org_idempotency,priority:2" json:"idempotency_key"`
	AmountMinor       int64               `gorm:"not null" json:"amount_minor"`
	Currency          string              `gorm:"size:3;not null;default:'MYR'" json:"currency"`
	Status            PaymentIntentStatus `gorm:"size:30;not null;default:'pending';index" json:"status"`
	CheckoutURL       string              `gorm:"type:text" json:"checkout_url,omitempty"`
	QRPayload         string              `gorm:"type:text" json:"qr_payload,omitempty"`
	ExpiresAt         *time.Time          `gorm:"index" json:"expires_at,omitempty"`
	SucceededAt       *time.Time          `json:"succeeded_at,omitempty"`
	FailedAt          *time.Time          `json:"failed_at,omitempty"`
	FailureCode       string              `gorm:"size:100" json:"failure_code,omitempty"`
	FailureMessage    string              `gorm:"type:text" json:"failure_message,omitempty"`
	Metadata          JSONB               `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	Version           int64               `gorm:"not null;default:1" json:"version"`
	CreatedByID       *uuid.UUID          `gorm:"type:uuid" json:"created_by_id,omitempty"`

	ProviderAccount *PaymentProviderAccount `gorm:"foreignKey:ProviderAccountID" json:"provider_account,omitempty"`
	Invoice         *CommerceInvoice        `gorm:"foreignKey:InvoiceID" json:"invoice,omitempty"`
	Contact         *Contact                `gorm:"foreignKey:ContactID" json:"contact,omitempty"`
}

func (PaymentIntent) TableName() string {
	return "payment_intents"
}

// PaymentTransaction is append-only. It deliberately does not embed BaseModel.
type PaymentTransaction struct {
	ID                    uuid.UUID                `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	OrganizationID        uuid.UUID                `gorm:"type:uuid;not null;index;uniqueIndex:idx_payment_transactions_org_idempotency,priority:1" json:"organization_id"`
	ProviderAccountID     uuid.UUID                `gorm:"type:uuid;not null;index" json:"provider_account_id"`
	IntentID              *uuid.UUID               `gorm:"type:uuid;index" json:"intent_id,omitempty"`
	InvoiceID             *uuid.UUID               `gorm:"type:uuid;index" json:"invoice_id,omitempty"`
	OriginalTransactionID *uuid.UUID               `gorm:"type:uuid;index" json:"original_transaction_id,omitempty"`
	Type                  PaymentTransactionType   `gorm:"size:20;not null;index" json:"type"`
	ProviderTransactionID string                   `gorm:"size:255;index" json:"provider_transaction_id,omitempty"`
	ProviderEventID       string                   `gorm:"size:255;index" json:"provider_event_id,omitempty"`
	IdempotencyKey        string                   `gorm:"size:255;not null;uniqueIndex:idx_payment_transactions_org_idempotency,priority:2" json:"idempotency_key"`
	AmountMinor           int64                    `gorm:"not null" json:"amount_minor"`
	Currency              string                   `gorm:"size:3;not null;default:'MYR'" json:"currency"`
	Status                PaymentTransactionStatus `gorm:"size:20;not null;index" json:"status"`
	Metadata              JSONB                    `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	OccurredAt            time.Time                `gorm:"not null;index" json:"occurred_at"`
	CreatedAt             time.Time                `gorm:"not null;autoCreateTime" json:"created_at"`

	ProviderAccount     *PaymentProviderAccount `gorm:"foreignKey:ProviderAccountID" json:"provider_account,omitempty"`
	Intent              *PaymentIntent          `gorm:"foreignKey:IntentID" json:"intent,omitempty"`
	Invoice             *CommerceInvoice        `gorm:"foreignKey:InvoiceID" json:"invoice,omitempty"`
	OriginalTransaction *PaymentTransaction     `gorm:"foreignKey:OriginalTransactionID" json:"original_transaction,omitempty"`
}

func (PaymentTransaction) TableName() string {
	return "payment_transactions"
}

// PaymentWebhookEvent stores a redacted, deduplicated provider event.
type PaymentWebhookEvent struct {
	ID                uuid.UUID                 `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	OrganizationID    uuid.UUID                 `gorm:"type:uuid;not null;index;uniqueIndex:idx_payment_webhook_provider_event,priority:1" json:"organization_id"`
	ProviderAccountID *uuid.UUID                `gorm:"type:uuid;index" json:"provider_account_id,omitempty"`
	Provider          string                    `gorm:"size:50;not null;index;uniqueIndex:idx_payment_webhook_provider_event,priority:2" json:"provider"`
	ProviderEventID   string                    `gorm:"size:255;not null;uniqueIndex:idx_payment_webhook_provider_event,priority:3" json:"provider_event_id"`
	PayloadHash       string                    `gorm:"size:128;not null;index" json:"payload_hash"`
	SignatureValid    bool                      `gorm:"not null;default:false" json:"signature_valid"`
	Status            PaymentWebhookEventStatus `gorm:"size:20;not null;default:'received';index" json:"status"`
	Attempts          int                       `gorm:"not null;default:0" json:"attempts"`
	LastError         string                    `gorm:"type:text" json:"last_error,omitempty"`
	RedactedPayload   JSONB                     `gorm:"type:jsonb;default:'{}'" json:"redacted_payload"`
	ReceivedAt        time.Time                 `gorm:"not null;autoCreateTime;index" json:"received_at"`
	ProcessedAt       *time.Time                `json:"processed_at,omitempty"`
	Version           int64                     `gorm:"not null;default:1" json:"version"`
	CreatedAt         time.Time                 `gorm:"not null;autoCreateTime" json:"created_at"`
	UpdatedAt         time.Time                 `gorm:"not null;autoUpdateTime" json:"updated_at"`

	ProviderAccount *PaymentProviderAccount `gorm:"foreignKey:ProviderAccountID" json:"provider_account,omitempty"`
}

func (PaymentWebhookEvent) TableName() string {
	return "payment_webhook_events"
}

// CopilotSettings configures human-in-the-loop AI assistance.
type CopilotSettings struct {
	BaseModel
	OrganizationID      uuid.UUID   `gorm:"type:uuid;not null;index" json:"organization_id"`
	WhatsAppAccount     string      `gorm:"size:100;index" json:"whatsapp_account,omitempty"`
	IsEnabled           bool        `gorm:"not null;default:false" json:"is_enabled"`
	Provider            AIProvider  `gorm:"size:20;not null;default:'qwen'" json:"provider"`
	Model               string      `gorm:"size:100;not null;default:'qwen3.7-plus'" json:"model"`
	APIKeyEncrypted     string      `gorm:"type:text" json:"-"`
	MaxTokens           int         `gorm:"not null;default:500" json:"max_tokens"`
	Temperature         float64     `gorm:"type:decimal(3,2);not null;default:0.3" json:"temperature"`
	SystemPrompt        string      `gorm:"type:text" json:"system_prompt,omitempty"`
	AllowedCapabilities StringArray `gorm:"type:jsonb;default:'[]'" json:"allowed_capabilities"`
	Guardrails          JSONB       `gorm:"type:jsonb;default:'{}'" json:"guardrails"`
	RetentionDays       int         `gorm:"not null;default:30" json:"retention_days"`
	Version             int64       `gorm:"not null;default:1" json:"version"`
	CreatedByID         *uuid.UUID  `gorm:"type:uuid" json:"created_by_id,omitempty"`
	UpdatedByID         *uuid.UUID  `gorm:"type:uuid" json:"updated_by_id,omitempty"`
}

func (CopilotSettings) TableName() string {
	return "copilot_settings"
}

// CopilotRun records one user-requested Copilot operation.
type CopilotRun struct {
	BaseModel
	OrganizationID     uuid.UUID        `gorm:"type:uuid;not null;index;uniqueIndex:idx_copilot_runs_org_idempotency,priority:1" json:"organization_id"`
	ContactID          uuid.UUID        `gorm:"type:uuid;not null;index" json:"contact_id"`
	RequestedByID      uuid.UUID        `gorm:"type:uuid;not null;index" json:"requested_by_id"`
	TaskType           CopilotTaskType  `gorm:"size:30;not null;index" json:"task_type"`
	Status             CopilotRunStatus `gorm:"size:20;not null;default:'pending';index" json:"status"`
	Model              string           `gorm:"size:100;not null" json:"model"`
	PromptVersion      string           `gorm:"size:50" json:"prompt_version,omitempty"`
	InputMessageIDs    StringArray      `gorm:"type:jsonb;default:'[]'" json:"input_message_ids"`
	InputHash          string           `gorm:"size:128;index" json:"input_hash,omitempty"`
	ResultText         string           `gorm:"type:text" json:"result_text,omitempty"`
	ResultData         JSONB            `gorm:"type:jsonb;default:'{}'" json:"result_data"`
	ContextSourceIDs   StringArray      `gorm:"type:jsonb;default:'[]'" json:"context_source_ids"`
	ContextSourceNames StringArray      `gorm:"type:jsonb;default:'[]'" json:"context_source_names"`
	SafetyLabels       StringArray      `gorm:"type:jsonb;default:'[]'" json:"safety_labels"`
	Warnings           StringArray      `gorm:"type:jsonb;default:'[]'" json:"warnings"`
	PromptTokens       int              `gorm:"not null;default:0" json:"prompt_tokens"`
	CompletionTokens   int              `gorm:"not null;default:0" json:"completion_tokens"`
	LatencyMS          int64            `gorm:"not null;default:0" json:"latency_ms"`
	ErrorCode          string           `gorm:"size:100" json:"error_code,omitempty"`
	ErrorMessage       string           `gorm:"type:text" json:"error_message,omitempty"`
	IdempotencyKey     string           `gorm:"size:255;not null;uniqueIndex:idx_copilot_runs_org_idempotency,priority:2" json:"idempotency_key"`
	ExpiresAt          *time.Time       `gorm:"index" json:"expires_at,omitempty"`
	Version            int64            `gorm:"not null;default:1" json:"version"`

	Contact     *Contact          `gorm:"foreignKey:ContactID" json:"contact,omitempty"`
	RequestedBy *User             `gorm:"foreignKey:RequestedByID" json:"requested_by,omitempty"`
	Feedback    []CopilotFeedback `gorm:"foreignKey:RunID" json:"feedback,omitempty"`
}

func (CopilotRun) TableName() string {
	return "copilot_runs"
}

// CopilotFeedback records whether an agent found and used a Copilot result.
type CopilotFeedback struct {
	BaseModel
	OrganizationID uuid.UUID             `gorm:"type:uuid;not null;index;uniqueIndex:idx_copilot_feedback_run_user,priority:1" json:"organization_id"`
	RunID          uuid.UUID             `gorm:"type:uuid;not null;index;uniqueIndex:idx_copilot_feedback_run_user,priority:2" json:"run_id"`
	UserID         uuid.UUID             `gorm:"type:uuid;not null;index;uniqueIndex:idx_copilot_feedback_run_user,priority:3" json:"user_id"`
	Rating         CopilotFeedbackRating `gorm:"size:20;index" json:"rating,omitempty"`
	Accepted       *bool                 `json:"accepted,omitempty"`
	FinalMessageID *uuid.UUID            `gorm:"type:uuid;index" json:"final_message_id,omitempty"`
	EditDistance   *float64              `gorm:"type:decimal(8,5)" json:"edit_distance,omitempty"`
	Reason         string                `gorm:"type:text" json:"reason,omitempty"`
	Metadata       JSONB                 `gorm:"type:jsonb;default:'{}'" json:"metadata"`
	Version        int64                 `gorm:"not null;default:1" json:"version"`

	Run          *CopilotRun `gorm:"foreignKey:RunID" json:"run,omitempty"`
	User         *User       `gorm:"foreignKey:UserID" json:"user,omitempty"`
	FinalMessage *Message    `gorm:"foreignKey:FinalMessageID" json:"final_message,omitempty"`
}

func (CopilotFeedback) TableName() string {
	return "copilot_feedback"
}
