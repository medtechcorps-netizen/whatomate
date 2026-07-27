package models

import (
	"time"

	"github.com/google/uuid"
)

// CommercialPlanStatus represents the lifecycle of a sellable plan.
type CommercialPlanStatus string

const (
	CommercialPlanStatusDraft    CommercialPlanStatus = "draft"
	CommercialPlanStatusActive   CommercialPlanStatus = "active"
	CommercialPlanStatusArchived CommercialPlanStatus = "archived"
)

// BillingProvider identifies the system responsible for collecting payment.
// Manual remains useful for bank-transfer and invoiced enterprise customers.
type BillingProvider string

const (
	BillingProviderManual  BillingProvider = "manual"
	BillingProviderStripe  BillingProvider = "stripe"
	BillingProviderXendit  BillingProvider = "xendit"
	BillingProviderBillplz BillingProvider = "billplz"
)

// BillingInterval is the recurrence period for a plan price.
type BillingInterval string

const (
	BillingIntervalOneTime BillingInterval = "one_time"
	BillingIntervalMonth   BillingInterval = "month"
	BillingIntervalYear    BillingInterval = "year"
)

// EntitlementValueType documents how a plan entitlement value is interpreted.
type EntitlementValueType string

const (
	EntitlementValueTypeBoolean EntitlementValueType = "boolean"
	EntitlementValueTypeInteger EntitlementValueType = "integer"
	EntitlementValueTypeString  EntitlementValueType = "string"
	EntitlementValueTypeJSON    EntitlementValueType = "json"
)

// EntitlementEnforcement controls the behavior when a limit is exceeded.
type EntitlementEnforcement string

const (
	EntitlementEnforcementNone EntitlementEnforcement = "none"
	EntitlementEnforcementSoft EntitlementEnforcement = "soft"
	EntitlementEnforcementHard EntitlementEnforcement = "hard"
)

// BillingAccountStatus represents the commercial standing of an organization.
type BillingAccountStatus string

const (
	BillingAccountStatusActive     BillingAccountStatus = "active"
	BillingAccountStatusDelinquent BillingAccountStatus = "delinquent"
	BillingAccountStatusSuspended  BillingAccountStatus = "suspended"
	BillingAccountStatusClosed     BillingAccountStatus = "closed"
)

// SubscriptionStatus represents the normalized lifecycle across billing providers.
type SubscriptionStatus string

const (
	SubscriptionStatusIncomplete SubscriptionStatus = "incomplete"
	SubscriptionStatusTrialing   SubscriptionStatus = "trialing"
	SubscriptionStatusActive     SubscriptionStatus = "active"
	SubscriptionStatusPastDue    SubscriptionStatus = "past_due"
	SubscriptionStatusPaused     SubscriptionStatus = "paused"
	SubscriptionStatusCanceled   SubscriptionStatus = "canceled"
	SubscriptionStatusExpired    SubscriptionStatus = "expired"
)

// InvoiceStatus represents the normalized invoice lifecycle.
type InvoiceStatus string

const (
	InvoiceStatusDraft         InvoiceStatus = "draft"
	InvoiceStatusOpen          InvoiceStatus = "open"
	InvoiceStatusPaid          InvoiceStatus = "paid"
	InvoiceStatusVoid          InvoiceStatus = "void"
	InvoiceStatusUncollectible InvoiceStatus = "uncollectible"
	InvoiceStatusRefunded      InvoiceStatus = "refunded"
)

// BillingWebhookStatus records processing state for an idempotent provider event.
type BillingWebhookStatus string

const (
	BillingWebhookStatusPending    BillingWebhookStatus = "pending"
	BillingWebhookStatusProcessing BillingWebhookStatus = "processing"
	BillingWebhookStatusProcessed  BillingWebhookStatus = "processed"
	BillingWebhookStatusFailed     BillingWebhookStatus = "failed"
	BillingWebhookStatusIgnored    BillingWebhookStatus = "ignored"
)

// BillingRollupStatus represents the lifecycle of a metered-usage period.
type BillingRollupStatus string

const (
	BillingRollupStatusOpen      BillingRollupStatus = "open"
	BillingRollupStatusFinalized BillingRollupStatus = "finalized"
	BillingRollupStatusInvoiced  BillingRollupStatus = "invoiced"
)

// Plan is a platform- or reseller-owned package of features. ScopeKey is a
// non-null stable owner key ("platform" or a reseller UUID) so PostgreSQL can
// enforce uniqueness without nullable-index edge cases.
type Plan struct {
	BaseModel
	ResellerID   *uuid.UUID           `gorm:"type:uuid;index" json:"reseller_id,omitempty"`
	ScopeKey     string               `gorm:"size:100;not null;uniqueIndex:idx_plan_scope_code" json:"scope_key"`
	Code         string               `gorm:"size:100;not null;uniqueIndex:idx_plan_scope_code" json:"code"`
	Name         string               `gorm:"size:255;not null" json:"name"`
	Description  string               `gorm:"type:text" json:"description"`
	Status       CommercialPlanStatus `gorm:"size:20;not null;default:'draft';index" json:"status"`
	Vertical     string               `gorm:"size:50;not null;default:'general';index" json:"vertical"`
	TrialDays    int                  `gorm:"not null;default:0" json:"trial_days"`
	DisplayOrder int                  `gorm:"not null;default:0" json:"display_order"`
	IsPublic     bool                 `gorm:"not null;default:false" json:"is_public"`
	Metadata     JSONB                `gorm:"type:jsonb;not null;default:'{}'" json:"metadata"`
	PublishedAt  *time.Time           `json:"published_at,omitempty"`
	ArchivedAt   *time.Time           `json:"archived_at,omitempty"`
	CreatedByID  *uuid.UUID           `gorm:"type:uuid;index" json:"created_by_id,omitempty"`
}

func (Plan) TableName() string {
	return "plans"
}

// PlanPrice stores monetary values in the currency's minor unit (for example,
// sen for MYR and cents for USD). ProviderData can include provider payloads
// and is intentionally never returned by model JSON serialization.
type PlanPrice struct {
	BaseModel
	PlanID           uuid.UUID       `gorm:"type:uuid;not null;index;uniqueIndex:idx_plan_price_code" json:"plan_id"`
	Code             string          `gorm:"size:100;not null;uniqueIndex:idx_plan_price_code" json:"code"`
	Provider         BillingProvider `gorm:"size:30;not null;default:'manual';index" json:"provider"`
	ProviderPriceID  string          `gorm:"size:255;index" json:"-"`
	Currency         string          `gorm:"size:3;not null" json:"currency"`
	UnitAmountMinor  int64           `gorm:"not null;default:0" json:"unit_amount_minor"`
	SetupAmountMinor int64           `gorm:"not null;default:0" json:"setup_amount_minor"`
	Interval         BillingInterval `gorm:"size:20;not null;default:'month'" json:"interval"`
	IntervalCount    int             `gorm:"not null;default:1" json:"interval_count"`
	TaxBehavior      string          `gorm:"size:20;not null;default:'exclusive'" json:"tax_behavior"`
	IsActive         bool            `gorm:"not null;default:true;index" json:"is_active"`
	EffectiveFrom    *time.Time      `json:"effective_from,omitempty"`
	EffectiveUntil   *time.Time      `json:"effective_until,omitempty"`
	ProviderData     JSONB           `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	Metadata         JSONB           `gorm:"type:jsonb;not null;default:'{}'" json:"metadata"`
}

func (PlanPrice) TableName() string {
	return "plan_prices"
}

// PlanEntitlement defines a feature flag, quota, or configuration included in
// a plan. Value is typed by ValueType; numeric quotas should be integral.
type PlanEntitlement struct {
	BaseModel
	PlanID                 uuid.UUID              `gorm:"type:uuid;not null;index;uniqueIndex:idx_plan_entitlement_key" json:"plan_id"`
	Key                    string                 `gorm:"size:150;not null;uniqueIndex:idx_plan_entitlement_key" json:"key"`
	ValueType              EntitlementValueType   `gorm:"size:20;not null" json:"value_type"`
	Value                  JSONB                  `gorm:"type:jsonb;not null;default:'{}'" json:"value"`
	Enforcement            EntitlementEnforcement `gorm:"size:20;not null;default:'hard'" json:"enforcement"`
	ResetInterval          BillingInterval        `gorm:"size:20" json:"reset_interval,omitempty"`
	OverageUnitAmountMinor int64                  `gorm:"not null;default:0" json:"overage_unit_amount_minor"`
	OverageCurrency        string                 `gorm:"size:3" json:"overage_currency,omitempty"`
	Description            string                 `gorm:"type:text" json:"description"`
}

func (PlanEntitlement) TableName() string {
	return "plan_entitlements"
}

// BillingAccount is the organization-level billing identity. Provider customer
// references, tax identifiers, and raw billing profile data are not serialized.
type BillingAccount struct {
	BaseModel
	OrganizationID     uuid.UUID            `gorm:"type:uuid;not null;uniqueIndex" json:"organization_id"`
	ResellerID         *uuid.UUID           `gorm:"type:uuid;index" json:"reseller_id,omitempty"`
	Provider           BillingProvider      `gorm:"size:30;not null;default:'manual';index" json:"provider"`
	ProviderCustomerID string               `gorm:"size:255;index" json:"-"`
	Status             BillingAccountStatus `gorm:"size:20;not null;default:'active';index" json:"status"`
	BillingEmail       string               `gorm:"size:255" json:"billing_email"`
	LegalName          string               `gorm:"size:255" json:"legal_name"`
	TaxID              string               `gorm:"size:100" json:"-"`
	DefaultCurrency    string               `gorm:"size:3;not null;default:'MYR'" json:"default_currency"`
	BillingProfile     JSONB                `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	ProviderData       JSONB                `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	Metadata           JSONB                `gorm:"type:jsonb;not null;default:'{}'" json:"metadata"`
	DelinquentAt       *time.Time           `json:"delinquent_at,omitempty"`
	SuspendedAt        *time.Time           `json:"suspended_at,omitempty"`
	ClosedAt           *time.Time           `json:"closed_at,omitempty"`
}

func (BillingAccount) TableName() string {
	return "billing_accounts"
}

// Subscription binds an organization to one plan price. EntitlementsSnapshot
// freezes the effective commercial terms for deterministic enforcement.
type Subscription struct {
	BaseModel
	OrganizationID         uuid.UUID          `gorm:"type:uuid;not null;index" json:"organization_id"`
	BillingAccountID       uuid.UUID          `gorm:"type:uuid;not null;index" json:"billing_account_id"`
	PlanID                 uuid.UUID          `gorm:"type:uuid;not null;index" json:"plan_id"`
	PlanPriceID            *uuid.UUID         `gorm:"type:uuid;index" json:"plan_price_id,omitempty"`
	Provider               BillingProvider    `gorm:"size:30;not null;default:'manual';index" json:"provider"`
	ProviderSubscriptionID string             `gorm:"size:255;index" json:"-"`
	Status                 SubscriptionStatus `gorm:"size:20;not null;default:'incomplete';index" json:"status"`
	Quantity               int                `gorm:"not null;default:1" json:"quantity"`
	CollectionMethod       string             `gorm:"size:30;not null;default:'charge_automatically'" json:"collection_method"`
	EntitlementsSnapshot   JSONB              `gorm:"type:jsonb;not null;default:'{}'" json:"entitlements_snapshot"`
	ProviderData           JSONB              `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	CurrentPeriodStart     *time.Time         `gorm:"index" json:"current_period_start,omitempty"`
	CurrentPeriodEnd       *time.Time         `gorm:"index" json:"current_period_end,omitempty"`
	TrialEndsAt            *time.Time         `json:"trial_ends_at,omitempty"`
	GraceUntil             *time.Time         `gorm:"index" json:"grace_until,omitempty"`
	CancelAtPeriodEnd      bool               `gorm:"not null;default:false" json:"cancel_at_period_end"`
	CancelAt               *time.Time         `json:"cancel_at,omitempty"`
	CanceledAt             *time.Time         `json:"canceled_at,omitempty"`
	EndedAt                *time.Time         `json:"ended_at,omitempty"`
	CreatedByID            *uuid.UUID         `gorm:"type:uuid;index" json:"created_by_id,omitempty"`
}

func (Subscription) TableName() string {
	return "subscriptions"
}

// Invoice stores a normalized ledger view. Every amount is expressed in minor
// units; hosted URLs and provider bodies are hidden because URLs may be bearer
// links and payloads may contain payment or personal data.
type Invoice struct {
	BaseModel
	OrganizationID       uuid.UUID       `gorm:"type:uuid;not null;index" json:"organization_id"`
	BillingAccountID     uuid.UUID       `gorm:"type:uuid;not null;index" json:"billing_account_id"`
	SubscriptionID       *uuid.UUID      `gorm:"type:uuid;index" json:"subscription_id,omitempty"`
	Provider             BillingProvider `gorm:"size:30;not null;default:'manual';index" json:"provider"`
	ProviderInvoiceID    string          `gorm:"size:255;index" json:"-"`
	Number               string          `gorm:"size:100;index" json:"number"`
	Status               InvoiceStatus   `gorm:"size:20;not null;default:'draft';index" json:"status"`
	Currency             string          `gorm:"size:3;not null" json:"currency"`
	SubtotalMinor        int64           `gorm:"not null;default:0" json:"subtotal_minor"`
	DiscountMinor        int64           `gorm:"not null;default:0" json:"discount_minor"`
	TaxMinor             int64           `gorm:"not null;default:0" json:"tax_minor"`
	TotalMinor           int64           `gorm:"not null;default:0" json:"total_minor"`
	AmountDueMinor       int64           `gorm:"not null;default:0" json:"amount_due_minor"`
	AmountPaidMinor      int64           `gorm:"not null;default:0" json:"amount_paid_minor"`
	AmountRemainingMinor int64           `gorm:"not null;default:0" json:"amount_remaining_minor"`
	AmountRefundedMinor  int64           `gorm:"not null;default:0" json:"amount_refunded_minor"`
	LineItems            JSONBArray      `gorm:"type:jsonb;not null;default:'[]'" json:"line_items"`
	ProviderData         JSONB           `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	HostedInvoiceURL     string          `gorm:"type:text" json:"-"`
	PDFObjectKey         string          `gorm:"type:text" json:"-"`
	PeriodStart          *time.Time      `json:"period_start,omitempty"`
	PeriodEnd            *time.Time      `json:"period_end,omitempty"`
	IssuedAt             *time.Time      `gorm:"index" json:"issued_at,omitempty"`
	DueAt                *time.Time      `gorm:"index" json:"due_at,omitempty"`
	PaidAt               *time.Time      `json:"paid_at,omitempty"`
	VoidedAt             *time.Time      `json:"voided_at,omitempty"`
}

func (Invoice) TableName() string {
	return "invoices"
}

// BillingWebhookEvent is the durable, idempotent inbox for provider callbacks.
// Request bodies, headers, and signatures are persisted for verification and
// replay but are never serialized to API clients.
type BillingWebhookEvent struct {
	BaseModel
	Provider        BillingProvider      `gorm:"size:30;not null;uniqueIndex:idx_billing_webhook_event" json:"provider"`
	ExternalEventID string               `gorm:"size:255;not null;uniqueIndex:idx_billing_webhook_event" json:"external_event_id"`
	OrganizationID  *uuid.UUID           `gorm:"type:uuid;index" json:"organization_id,omitempty"`
	ResellerID      *uuid.UUID           `gorm:"type:uuid;index" json:"reseller_id,omitempty"`
	EventType       string               `gorm:"size:150;not null;index" json:"event_type"`
	Status          BillingWebhookStatus `gorm:"size:20;not null;default:'pending';index" json:"status"`
	AttemptCount    int                  `gorm:"not null;default:0" json:"attempt_count"`
	Payload         JSONB                `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	Headers         JSONB                `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	Signature       string               `gorm:"type:text" json:"-"`
	PayloadHash     string               `gorm:"size:128" json:"payload_hash,omitempty"`
	ReceivedAt      time.Time            `gorm:"not null;index" json:"received_at"`
	ProcessingAt    *time.Time           `json:"processing_at,omitempty"`
	ProcessedAt     *time.Time           `json:"processed_at,omitempty"`
	NextAttemptAt   *time.Time           `gorm:"index" json:"next_attempt_at,omitempty"`
	LastError       string               `gorm:"type:text" json:"last_error,omitempty"`
}

func (BillingWebhookEvent) TableName() string {
	return "billing_webhook_events"
}

// EntitlementOverride grants or restricts an organization independently of its
// subscription. Multiple time-bounded records preserve an auditable history.
type EntitlementOverride struct {
	BaseModel
	OrganizationID   uuid.UUID            `gorm:"type:uuid;not null;index:idx_entitlement_override_lookup" json:"organization_id"`
	Key              string               `gorm:"size:150;not null;index:idx_entitlement_override_lookup" json:"key"`
	ValueType        EntitlementValueType `gorm:"size:20;not null" json:"value_type"`
	Value            JSONB                `gorm:"type:jsonb;not null;default:'{}'" json:"value"`
	Source           string               `gorm:"size:30;not null;default:'support'" json:"source"`
	Reason           string               `gorm:"type:text;not null" json:"reason"`
	IsActive         bool                 `gorm:"not null;default:true;index" json:"is_active"`
	StartsAt         time.Time            `gorm:"not null;index" json:"starts_at"`
	ExpiresAt        *time.Time           `gorm:"index" json:"expires_at,omitempty"`
	CreatedByID      uuid.UUID            `gorm:"type:uuid;not null;index" json:"created_by_id"`
	RevokedByID      *uuid.UUID           `gorm:"type:uuid;index" json:"revoked_by_id,omitempty"`
	RevokedAt        *time.Time           `json:"revoked_at,omitempty"`
	RevocationReason string               `gorm:"type:text" json:"revocation_reason,omitempty"`
}

func (EntitlementOverride) TableName() string {
	return "entitlement_overrides"
}

// UsageEvent is an append-only metering event. IdempotencyKey is unique within
// an organization so adapter retries cannot double bill a customer.
type UsageEvent struct {
	BaseModel
	OrganizationID uuid.UUID  `gorm:"type:uuid;not null;index;uniqueIndex:idx_usage_event_idempotency" json:"organization_id"`
	SubscriptionID *uuid.UUID `gorm:"type:uuid;index" json:"subscription_id,omitempty"`
	EntitlementKey string     `gorm:"size:150;not null;index" json:"entitlement_key"`
	IdempotencyKey string     `gorm:"size:255;not null;uniqueIndex:idx_usage_event_idempotency" json:"idempotency_key"`
	Quantity       int64      `gorm:"not null" json:"quantity"`
	Source         string     `gorm:"size:50;not null;default:'application';index" json:"source"`
	ReferenceType  string     `gorm:"size:100" json:"reference_type,omitempty"`
	ReferenceID    string     `gorm:"size:255" json:"reference_id,omitempty"`
	Dimensions     JSONB      `gorm:"type:jsonb;not null;default:'{}'" json:"dimensions"`
	OccurredAt     time.Time  `gorm:"not null;index" json:"occurred_at"`
	IngestedAt     time.Time  `gorm:"not null;index" json:"ingested_at"`
}

func (UsageEvent) TableName() string {
	return "usage_events"
}

// BillingUsageRollup is the deterministic aggregate used for enforcement and
// invoicing. Amounts are minor units in Currency.
type BillingUsageRollup struct {
	BaseModel
	OrganizationID      uuid.UUID           `gorm:"type:uuid;not null;index;uniqueIndex:idx_usage_rollup_period" json:"organization_id"`
	SubscriptionID      *uuid.UUID          `gorm:"type:uuid;index" json:"subscription_id,omitempty"`
	EntitlementKey      string              `gorm:"size:150;not null;uniqueIndex:idx_usage_rollup_period" json:"entitlement_key"`
	PeriodStart         time.Time           `gorm:"not null;uniqueIndex:idx_usage_rollup_period" json:"period_start"`
	PeriodEnd           time.Time           `gorm:"not null;uniqueIndex:idx_usage_rollup_period" json:"period_end"`
	Status              BillingRollupStatus `gorm:"size:20;not null;default:'open';index" json:"status"`
	Quantity            int64               `gorm:"not null;default:0" json:"quantity"`
	IncludedQuantity    int64               `gorm:"not null;default:0" json:"included_quantity"`
	BillableQuantity    int64               `gorm:"not null;default:0" json:"billable_quantity"`
	OverageAmountMinor  int64               `gorm:"not null;default:0" json:"overage_amount_minor"`
	Currency            string              `gorm:"size:3" json:"currency,omitempty"`
	InvoiceID           *uuid.UUID          `gorm:"type:uuid;index" json:"invoice_id,omitempty"`
	LastEventOccurredAt *time.Time          `json:"last_event_occurred_at,omitempty"`
	FinalizedAt         *time.Time          `json:"finalized_at,omitempty"`
}

func (BillingUsageRollup) TableName() string {
	return "billing_usage_rollups"
}

// OnboardingStatus represents the customer workspace activation lifecycle.
type OnboardingStatus string

const (
	OnboardingStatusNotStarted   OnboardingStatus = "not_started"
	OnboardingStatusInProgress   OnboardingStatus = "in_progress"
	OnboardingStatusProvisioning OnboardingStatus = "provisioning"
	OnboardingStatusReady        OnboardingStatus = "ready"
	OnboardingStatusFailed       OnboardingStatus = "failed"
	OnboardingStatusCanceled     OnboardingStatus = "canceled"
)

// ProvisioningStatus represents one idempotent provisioning attempt.
type ProvisioningStatus string

const (
	ProvisioningStatusQueued    ProvisioningStatus = "queued"
	ProvisioningStatusRunning   ProvisioningStatus = "running"
	ProvisioningStatusSucceeded ProvisioningStatus = "succeeded"
	ProvisioningStatusFailed    ProvisioningStatus = "failed"
	ProvisioningStatusCanceled  ProvisioningStatus = "canceled"
)

// WorkspaceTemplateStatus represents template/catalog publication state.
type WorkspaceTemplateStatus string

const (
	WorkspaceTemplateStatusDraft      WorkspaceTemplateStatus = "draft"
	WorkspaceTemplateStatusPublished  WorkspaceTemplateStatus = "published"
	WorkspaceTemplateStatusDeprecated WorkspaceTemplateStatus = "deprecated"
	WorkspaceTemplateStatusArchived   WorkspaceTemplateStatus = "archived"
)

// TemplateApplicationStatus represents an installation or reconciliation run.
type TemplateApplicationStatus string

const (
	TemplateApplicationStatusPending    TemplateApplicationStatus = "pending"
	TemplateApplicationStatusApplying   TemplateApplicationStatus = "applying"
	TemplateApplicationStatusSucceeded  TemplateApplicationStatus = "succeeded"
	TemplateApplicationStatusFailed     TemplateApplicationStatus = "failed"
	TemplateApplicationStatusRolledBack TemplateApplicationStatus = "rolled_back"
)

// OrganizationOnboarding is the resumable state machine for one organization.
type OrganizationOnboarding struct {
	BaseModel
	OrganizationID    uuid.UUID        `gorm:"type:uuid;not null;uniqueIndex" json:"organization_id"`
	ResellerID        *uuid.UUID       `gorm:"type:uuid;index" json:"reseller_id,omitempty"`
	TemplateID        *uuid.UUID       `gorm:"type:uuid;index" json:"template_id,omitempty"`
	TemplateVersionID *uuid.UUID       `gorm:"type:uuid;index" json:"template_version_id,omitempty"`
	Status            OnboardingStatus `gorm:"size:30;not null;default:'not_started';index" json:"status"`
	CurrentStep       string           `gorm:"size:100" json:"current_step,omitempty"`
	ProgressPercent   int              `gorm:"not null;default:0" json:"progress_percent"`
	Checklist         JSONB            `gorm:"type:jsonb;not null;default:'{}'" json:"checklist"`
	Input             JSONB            `gorm:"type:jsonb;not null;default:'{}'" json:"input"`
	Metadata          JSONB            `gorm:"type:jsonb;not null;default:'{}'" json:"metadata"`
	RequestedByID     *uuid.UUID       `gorm:"type:uuid;index" json:"requested_by_id,omitempty"`
	OwnerUserID       *uuid.UUID       `gorm:"type:uuid;index" json:"owner_user_id,omitempty"`
	StartedAt         *time.Time       `json:"started_at,omitempty"`
	CompletedAt       *time.Time       `json:"completed_at,omitempty"`
	LastError         string           `gorm:"type:text" json:"last_error,omitempty"`
}

func (OrganizationOnboarding) TableName() string {
	return "organization_onboardings"
}

// ProvisioningRun records one asynchronous onboarding attempt. Steps must
// contain sanitized execution summaries rather than credentials or raw payloads.
type ProvisioningRun struct {
	BaseModel
	OrganizationID    uuid.UUID          `gorm:"type:uuid;not null;index" json:"organization_id"`
	OnboardingID      uuid.UUID          `gorm:"type:uuid;not null;index" json:"onboarding_id"`
	TemplateVersionID *uuid.UUID         `gorm:"type:uuid;index" json:"template_version_id,omitempty"`
	Kind              string             `gorm:"size:30;not null;default:'initial'" json:"kind"`
	IdempotencyKey    string             `gorm:"size:255;not null;uniqueIndex" json:"idempotency_key"`
	Status            ProvisioningStatus `gorm:"size:20;not null;default:'queued';index" json:"status"`
	Attempt           int                `gorm:"not null;default:1" json:"attempt"`
	Steps             JSONBArray         `gorm:"type:jsonb;not null;default:'[]'" json:"steps"`
	WorkerJobID       string             `gorm:"size:255;index" json:"worker_job_id,omitempty"`
	RequestedByID     *uuid.UUID         `gorm:"type:uuid;index" json:"requested_by_id,omitempty"`
	StartedAt         *time.Time         `json:"started_at,omitempty"`
	FinishedAt        *time.Time         `json:"finished_at,omitempty"`
	LastHeartbeatAt   *time.Time         `json:"last_heartbeat_at,omitempty"`
	ErrorCode         string             `gorm:"size:100" json:"error_code,omitempty"`
	ErrorMessage      string             `gorm:"type:text" json:"error_message,omitempty"`
}

func (ProvisioningRun) TableName() string {
	return "provisioning_runs"
}

// WorkspaceTemplate is a reusable vertical workspace definition. Version rows
// reference it; there is deliberately no reverse current-version foreign key.
type WorkspaceTemplate struct {
	BaseModel
	ResellerID  *uuid.UUID              `gorm:"type:uuid;index" json:"reseller_id,omitempty"`
	ScopeKey    string                  `gorm:"size:100;not null;uniqueIndex:idx_workspace_template_scope_slug" json:"scope_key"`
	Slug        string                  `gorm:"size:100;not null;uniqueIndex:idx_workspace_template_scope_slug" json:"slug"`
	Name        string                  `gorm:"size:255;not null" json:"name"`
	Description string                  `gorm:"type:text" json:"description"`
	Vertical    string                  `gorm:"size:50;not null;default:'general';index" json:"vertical"`
	Status      WorkspaceTemplateStatus `gorm:"size:20;not null;default:'draft';index" json:"status"`
	IsDefault   bool                    `gorm:"not null;default:false;index" json:"is_default"`
	MinPlanCode string                  `gorm:"size:100" json:"min_plan_code,omitempty"`
	Icon        string                  `gorm:"size:100" json:"icon,omitempty"`
	Tags        StringArray             `gorm:"type:jsonb;not null;default:'[]'" json:"tags"`
	Settings    JSONB                   `gorm:"type:jsonb;not null;default:'{}'" json:"settings"`
	CreatedByID *uuid.UUID              `gorm:"type:uuid;index" json:"created_by_id,omitempty"`
	PublishedAt *time.Time              `json:"published_at,omitempty"`
}

func (WorkspaceTemplate) TableName() string {
	return "workspace_templates"
}

// WorkspaceTemplateVersion is an immutable declarative provisioning manifest
// once published.
type WorkspaceTemplateVersion struct {
	BaseModel
	TemplateID    uuid.UUID               `gorm:"type:uuid;not null;index;uniqueIndex:idx_workspace_template_version" json:"template_id"`
	Version       int                     `gorm:"not null;uniqueIndex:idx_workspace_template_version" json:"version"`
	Status        WorkspaceTemplateStatus `gorm:"size:20;not null;default:'draft';index" json:"status"`
	SchemaVersion string                  `gorm:"size:30;not null;default:'1'" json:"schema_version"`
	Manifest      JSONB                   `gorm:"type:jsonb;not null;default:'{}'" json:"manifest"`
	ChangeSummary string                  `gorm:"type:text" json:"change_summary"`
	Checksum      string                  `gorm:"size:128;not null" json:"checksum"`
	CreatedByID   *uuid.UUID              `gorm:"type:uuid;index" json:"created_by_id,omitempty"`
	PublishedByID *uuid.UUID              `gorm:"type:uuid;index" json:"published_by_id,omitempty"`
	PublishedAt   *time.Time              `json:"published_at,omitempty"`
}

func (WorkspaceTemplateVersion) TableName() string {
	return "workspace_template_versions"
}

// WorkspaceTemplateApplication records the exact version and manifest applied
// to an organization, supporting safe reconcile and upgrade operations.
type WorkspaceTemplateApplication struct {
	BaseModel
	OrganizationID        uuid.UUID                 `gorm:"type:uuid;not null;index" json:"organization_id"`
	TemplateID            uuid.UUID                 `gorm:"type:uuid;not null;index" json:"template_id"`
	TemplateVersionID     uuid.UUID                 `gorm:"type:uuid;not null;index" json:"template_version_id"`
	OnboardingID          *uuid.UUID                `gorm:"type:uuid;index" json:"onboarding_id,omitempty"`
	ProvisioningRunID     *uuid.UUID                `gorm:"type:uuid;index" json:"provisioning_run_id,omitempty"`
	PreviousApplicationID *uuid.UUID                `gorm:"type:uuid;index" json:"previous_application_id,omitempty"`
	Mode                  string                    `gorm:"size:30;not null;default:'install'" json:"mode"`
	Status                TemplateApplicationStatus `gorm:"size:30;not null;default:'pending';index" json:"status"`
	ManifestSnapshot      JSONB                     `gorm:"type:jsonb;not null;default:'{}'" json:"manifest_snapshot"`
	RequestedByID         *uuid.UUID                `gorm:"type:uuid;index" json:"requested_by_id,omitempty"`
	RequestedAt           time.Time                 `gorm:"not null;index" json:"requested_at"`
	CompletedAt           *time.Time                `json:"completed_at,omitempty"`
	ErrorCode             string                    `gorm:"size:100" json:"error_code,omitempty"`
	ErrorMessage          string                    `gorm:"type:text" json:"error_message,omitempty"`
}

func (WorkspaceTemplateApplication) TableName() string {
	return "workspace_template_applications"
}

// WorkspaceTemplateResourceMap maps declarative keys to concrete resources.
// ResourceID is a string because some resources use UUIDs and others stable names.
type WorkspaceTemplateResourceMap struct {
	BaseModel
	OrganizationID      uuid.UUID `gorm:"type:uuid;not null;index" json:"organization_id"`
	ApplicationID       uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:idx_template_resource_key" json:"application_id"`
	TemplateResourceKey string    `gorm:"size:255;not null;uniqueIndex:idx_template_resource_key" json:"template_resource_key"`
	ResourceType        string    `gorm:"size:100;not null;index" json:"resource_type"`
	ResourceID          string    `gorm:"size:255;not null;index" json:"resource_id"`
	Action              string    `gorm:"size:30;not null;default:'created'" json:"action"`
	Status              string    `gorm:"size:30;not null;default:'active';index" json:"status"`
	SourceChecksum      string    `gorm:"size:128" json:"source_checksum,omitempty"`
	Metadata            JSONB     `gorm:"type:jsonb;not null;default:'{}'" json:"metadata"`
}

func (WorkspaceTemplateResourceMap) TableName() string {
	return "workspace_template_resource_maps"
}

// ConsentAction is the immutable action captured in a consent event.
type ConsentAction string

const (
	ConsentActionGranted   ConsentAction = "granted"
	ConsentActionDenied    ConsentAction = "denied"
	ConsentActionWithdrawn ConsentAction = "withdrawn"
	ConsentActionExpired   ConsentAction = "expired"
)

// ConsentStatus is the effective state for a subject, purpose, and channel.
type ConsentStatus string

const (
	ConsentStatusUnknown   ConsentStatus = "unknown"
	ConsentStatusGranted   ConsentStatus = "granted"
	ConsentStatusDenied    ConsentStatus = "denied"
	ConsentStatusWithdrawn ConsentStatus = "withdrawn"
	ConsentStatusExpired   ConsentStatus = "expired"
)

// RetentionAction is the terminal action for data past its retention period.
type RetentionAction string

const (
	RetentionActionDelete    RetentionAction = "delete"
	RetentionActionAnonymize RetentionAction = "anonymize"
	RetentionActionArchive   RetentionAction = "archive"
	RetentionActionReview    RetentionAction = "review"
)

// PrivacyRequestType represents common data-subject rights workflows.
type PrivacyRequestType string

const (
	PrivacyRequestTypeAccess      PrivacyRequestType = "access"
	PrivacyRequestTypeExport      PrivacyRequestType = "export"
	PrivacyRequestTypeCorrection  PrivacyRequestType = "correction"
	PrivacyRequestTypeDeletion    PrivacyRequestType = "deletion"
	PrivacyRequestTypeErasure     PrivacyRequestType = "erasure"
	PrivacyRequestTypeRestriction PrivacyRequestType = "restriction"
	PrivacyRequestTypePortability PrivacyRequestType = "portability"
	PrivacyRequestTypeObjection   PrivacyRequestType = "objection"
)

// PrivacyRequestStatus represents a privacy request's review lifecycle.
type PrivacyRequestStatus string

const (
	PrivacyRequestStatusReceived             PrivacyRequestStatus = "received"
	PrivacyRequestStatusAwaitingVerification PrivacyRequestStatus = "awaiting_verification"
	PrivacyRequestStatusVerified             PrivacyRequestStatus = "verified"
	PrivacyRequestStatusInProgress           PrivacyRequestStatus = "in_progress"
	PrivacyRequestStatusCompleted            PrivacyRequestStatus = "completed"
	PrivacyRequestStatusDenied               PrivacyRequestStatus = "denied"
	PrivacyRequestStatusCanceled             PrivacyRequestStatus = "canceled"
	PrivacyRequestStatusExpired              PrivacyRequestStatus = "expired"
)

// PrivacyJobStatus represents an asynchronous privacy operation.
type PrivacyJobStatus string

const (
	PrivacyJobStatusQueued    PrivacyJobStatus = "queued"
	PrivacyJobStatusRunning   PrivacyJobStatus = "running"
	PrivacyJobStatusSucceeded PrivacyJobStatus = "succeeded"
	PrivacyJobStatusFailed    PrivacyJobStatus = "failed"
	PrivacyJobStatusCanceled  PrivacyJobStatus = "canceled"
)

// LegalHoldStatus represents whether normal retention processing is suspended.
type LegalHoldStatus string

const (
	LegalHoldStatusActive   LegalHoldStatus = "active"
	LegalHoldStatusReleased LegalHoldStatus = "released"
)

// BreachIncidentStatus represents the response lifecycle for a suspected breach.
type BreachIncidentStatus string

const (
	BreachIncidentStatusOpen          BreachIncidentStatus = "open"
	BreachIncidentStatusInvestigating BreachIncidentStatus = "investigating"
	BreachIncidentStatusContained     BreachIncidentStatus = "contained"
	BreachIncidentStatusResolved      BreachIncidentStatus = "resolved"
	BreachIncidentStatusClosed        BreachIncidentStatus = "closed"
)

// ConsentEvent is an append-only source of truth. Evidence may contain message
// excerpts, IP addresses, or provider receipts and is therefore excluded from JSON.
type ConsentEvent struct {
	BaseModel
	OrganizationID uuid.UUID     `gorm:"type:uuid;not null;index" json:"organization_id"`
	ContactID      *uuid.UUID    `gorm:"type:uuid;index" json:"contact_id,omitempty"`
	SubjectType    string        `gorm:"size:50;not null;default:'contact'" json:"subject_type"`
	SubjectKey     string        `gorm:"size:255;not null;index" json:"subject_key"`
	Purpose        string        `gorm:"size:100;not null;index" json:"purpose"`
	Channel        string        `gorm:"size:50;not null;index" json:"channel"`
	Action         ConsentAction `gorm:"size:20;not null;index" json:"action"`
	LegalBasis     string        `gorm:"size:100" json:"legal_basis,omitempty"`
	PolicyVersion  string        `gorm:"size:100" json:"policy_version,omitempty"`
	Source         string        `gorm:"size:50;not null" json:"source"`
	ActorUserID    *uuid.UUID    `gorm:"type:uuid;index" json:"actor_user_id,omitempty"`
	CorrelationID  string        `gorm:"size:255;index" json:"correlation_id,omitempty"`
	Evidence       JSONB         `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	EvidenceHash   string        `gorm:"size:128" json:"evidence_hash,omitempty"`
	CapturedAt     time.Time     `gorm:"not null;index" json:"captured_at"`
	ExpiresAt      *time.Time    `gorm:"index" json:"expires_at,omitempty"`
}

func (ConsentEvent) TableName() string {
	return "consent_events"
}

// ConsentState is a materialized lookup derived from ConsentEvent.
type ConsentState struct {
	BaseModel
	OrganizationID uuid.UUID     `gorm:"type:uuid;not null;uniqueIndex:idx_consent_state_subject" json:"organization_id"`
	ContactID      *uuid.UUID    `gorm:"type:uuid;index" json:"contact_id,omitempty"`
	SubjectType    string        `gorm:"size:50;not null;default:'contact';uniqueIndex:idx_consent_state_subject" json:"subject_type"`
	SubjectKey     string        `gorm:"size:255;not null;uniqueIndex:idx_consent_state_subject" json:"subject_key"`
	Purpose        string        `gorm:"size:100;not null;uniqueIndex:idx_consent_state_subject" json:"purpose"`
	Channel        string        `gorm:"size:50;not null;uniqueIndex:idx_consent_state_subject" json:"channel"`
	Status         ConsentStatus `gorm:"size:20;not null;default:'unknown';index" json:"status"`
	LatestEventID  uuid.UUID     `gorm:"type:uuid;not null;index" json:"latest_event_id"`
	PolicyVersion  string        `gorm:"size:100" json:"policy_version,omitempty"`
	EffectiveAt    time.Time     `gorm:"not null" json:"effective_at"`
	ExpiresAt      *time.Time    `gorm:"index" json:"expires_at,omitempty"`
	Metadata       JSONB         `gorm:"type:jsonb;not null;default:'{}'" json:"metadata"`
}

func (ConsentState) TableName() string {
	return "consent_states"
}

// RetentionPolicy defines an organization's disposition rule per resource type.
type RetentionPolicy struct {
	BaseModel
	OrganizationID  uuid.UUID       `gorm:"type:uuid;not null;uniqueIndex:idx_retention_policy_resource" json:"organization_id"`
	ResourceType    string          `gorm:"size:100;not null;uniqueIndex:idx_retention_policy_resource" json:"resource_type"`
	Action          RetentionAction `gorm:"size:20;not null" json:"action"`
	RetentionDays   int             `gorm:"not null" json:"retention_days"`
	GracePeriodDays int             `gorm:"not null;default:0" json:"grace_period_days"`
	LegalBasis      string          `gorm:"type:text" json:"legal_basis"`
	IsActive        bool            `gorm:"not null;default:true;index" json:"is_active"`
	Exceptions      JSONB           `gorm:"type:jsonb;not null;default:'{}'" json:"exceptions"`
	CreatedByID     uuid.UUID       `gorm:"type:uuid;not null;index" json:"created_by_id"`
	ApprovedByID    *uuid.UUID      `gorm:"type:uuid;index" json:"approved_by_id,omitempty"`
	EffectiveFrom   time.Time       `gorm:"not null;index" json:"effective_from"`
	LastAppliedAt   *time.Time      `json:"last_applied_at,omitempty"`
}

func (RetentionPolicy) TableName() string {
	return "retention_policies"
}

// PrivacyRequest tracks a data-subject request without exposing verification
// material, requester PII, request bodies, or generated download locations.
type PrivacyRequest struct {
	BaseModel
	OrganizationID        uuid.UUID            `gorm:"type:uuid;not null;index" json:"organization_id"`
	RequestNumber         string               `gorm:"size:100;not null;uniqueIndex" json:"request_number"`
	Type                  PrivacyRequestType   `gorm:"size:30;not null;index" json:"type"`
	Status                PrivacyRequestStatus `gorm:"size:30;not null;default:'received';index" json:"status"`
	SubjectType           string               `gorm:"size:50;not null;default:'contact'" json:"subject_type"`
	SubjectKey            string               `gorm:"size:255;not null;index" json:"subject_key"`
	ContactID             *uuid.UUID           `gorm:"type:uuid;index" json:"contact_id,omitempty"`
	ReceivedChannel       string               `gorm:"size:50;not null" json:"received_channel"`
	RequesterProfile      JSONB                `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	RequestDetails        JSONB                `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	VerificationMethod    string               `gorm:"size:50" json:"verification_method,omitempty"`
	VerificationTokenHash string               `gorm:"size:255" json:"-"`
	AssignedToID          *uuid.UUID           `gorm:"type:uuid;index" json:"assigned_to_id,omitempty"`
	ApprovedByID          *uuid.UUID           `gorm:"type:uuid;index" json:"approved_by_id,omitempty"`
	ReceivedAt            time.Time            `gorm:"not null;index" json:"received_at"`
	DueAt                 time.Time            `gorm:"not null;index" json:"due_at"`
	VerifiedAt            *time.Time           `json:"verified_at,omitempty"`
	CompletedAt           *time.Time           `json:"completed_at,omitempty"`
	DecisionReason        string               `gorm:"type:text" json:"decision_reason,omitempty"`
	ResultObjectKey       string               `gorm:"type:text" json:"-"`
	ResultExpiresAt       *time.Time           `json:"result_expires_at,omitempty"`
}

func (PrivacyRequest) TableName() string {
	return "privacy_requests"
}

// PrivacyRequestEvent is the append-only human and system audit trail.
type PrivacyRequestEvent struct {
	BaseModel
	OrganizationID   uuid.UUID            `gorm:"type:uuid;not null;index" json:"organization_id"`
	PrivacyRequestID uuid.UUID            `gorm:"type:uuid;not null;index" json:"privacy_request_id"`
	EventType        string               `gorm:"size:100;not null;index" json:"event_type"`
	FromStatus       PrivacyRequestStatus `gorm:"size:30" json:"from_status,omitempty"`
	ToStatus         PrivacyRequestStatus `gorm:"size:30" json:"to_status,omitempty"`
	ActorUserID      *uuid.UUID           `gorm:"type:uuid;index" json:"actor_user_id,omitempty"`
	Message          string               `gorm:"type:text" json:"message,omitempty"`
	Details          JSONB                `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	OccurredAt       time.Time            `gorm:"not null;index" json:"occurred_at"`
}

func (PrivacyRequestEvent) TableName() string {
	return "privacy_request_events"
}

// PrivacyJob is one idempotent asynchronous discovery, export, or erasure task.
type PrivacyJob struct {
	BaseModel
	OrganizationID   uuid.UUID        `gorm:"type:uuid;not null;index" json:"organization_id"`
	PrivacyRequestID uuid.UUID        `gorm:"type:uuid;not null;index" json:"privacy_request_id"`
	Kind             string           `gorm:"size:50;not null;index" json:"kind"`
	Status           PrivacyJobStatus `gorm:"size:20;not null;default:'queued';index" json:"status"`
	IdempotencyKey   string           `gorm:"size:255;not null;uniqueIndex" json:"idempotency_key"`
	Attempt          int              `gorm:"not null;default:0" json:"attempt"`
	Statistics       JSONB            `gorm:"type:jsonb;not null;default:'{}'" json:"statistics"`
	Cursor           JSONB            `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	ResultObjectKey  string           `gorm:"type:text" json:"-"`
	EncryptionKeyRef string           `gorm:"size:255" json:"-"`
	ResultHash       string           `gorm:"size:128" json:"result_hash,omitempty"`
	QueuedAt         time.Time        `gorm:"not null;index" json:"queued_at"`
	StartedAt        *time.Time       `json:"started_at,omitempty"`
	FinishedAt       *time.Time       `json:"finished_at,omitempty"`
	NextAttemptAt    *time.Time       `gorm:"index" json:"next_attempt_at,omitempty"`
	ErrorCode        string           `gorm:"size:100" json:"error_code,omitempty"`
	ErrorMessage     string           `gorm:"type:text" json:"error_message,omitempty"`
}

func (PrivacyJob) TableName() string {
	return "privacy_jobs"
}

// LegalHold suspends normal disposition for a narrowly defined data scope.
type LegalHold struct {
	BaseModel
	OrganizationID uuid.UUID       `gorm:"type:uuid;not null;uniqueIndex:idx_legal_hold_code" json:"organization_id"`
	Code           string          `gorm:"size:100;not null;uniqueIndex:idx_legal_hold_code" json:"code"`
	Status         LegalHoldStatus `gorm:"size:20;not null;default:'active';index" json:"status"`
	Name           string          `gorm:"size:255;not null" json:"name"`
	Reason         string          `gorm:"type:text;not null" json:"reason"`
	Scope          JSONB           `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	PlacedByID     uuid.UUID       `gorm:"type:uuid;not null;index" json:"placed_by_id"`
	ApprovedByID   *uuid.UUID      `gorm:"type:uuid;index" json:"approved_by_id,omitempty"`
	StartsAt       time.Time       `gorm:"not null;index" json:"starts_at"`
	EndsAt         *time.Time      `gorm:"index" json:"ends_at,omitempty"`
	ReleasedByID   *uuid.UUID      `gorm:"type:uuid;index" json:"released_by_id,omitempty"`
	ReleasedAt     *time.Time      `json:"released_at,omitempty"`
	ReleaseReason  string          `gorm:"type:text" json:"release_reason,omitempty"`
}

func (LegalHold) TableName() string {
	return "legal_holds"
}

// BreachIncident captures response milestones. Investigation bodies and
// notification logs are sensitive operational payloads and are not serialized.
type BreachIncident struct {
	BaseModel
	OrganizationID          uuid.UUID            `gorm:"type:uuid;not null;index" json:"organization_id"`
	IncidentNumber          string               `gorm:"size:100;not null;uniqueIndex" json:"incident_number"`
	Status                  BreachIncidentStatus `gorm:"size:30;not null;default:'open';index" json:"status"`
	Severity                string               `gorm:"size:20;not null;default:'medium';index" json:"severity"`
	Title                   string               `gorm:"size:255;not null" json:"title"`
	Summary                 string               `gorm:"type:text;not null" json:"summary"`
	AffectedDataCategories  StringArray          `gorm:"type:jsonb;not null;default:'[]'" json:"affected_data_categories"`
	AffectedSubjectEstimate int64                `gorm:"not null;default:0" json:"affected_subject_estimate"`
	OwnerUserID             *uuid.UUID           `gorm:"type:uuid;index" json:"owner_user_id,omitempty"`
	ExternalReference       string               `gorm:"size:255" json:"external_reference,omitempty"`
	Investigation           JSONB                `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	NotificationLog         JSONBArray           `gorm:"type:jsonb;not null;default:'[]'" json:"-"`
	DetectedAt              time.Time            `gorm:"not null;index" json:"detected_at"`
	ContainedAt             *time.Time           `json:"contained_at,omitempty"`
	ResolvedAt              *time.Time           `json:"resolved_at,omitempty"`
	RegulatorNotifiedAt     *time.Time           `json:"regulator_notified_at,omitempty"`
	SubjectsNotifiedAt      *time.Time           `json:"subjects_notified_at,omitempty"`
}

func (BreachIncident) TableName() string {
	return "breach_incidents"
}

// SupportGrantStatus represents just-in-time privileged support access.
type SupportGrantStatus string

const (
	SupportGrantStatusPending SupportGrantStatus = "pending"
	SupportGrantStatusActive  SupportGrantStatus = "active"
	SupportGrantStatusRevoked SupportGrantStatus = "revoked"
	SupportGrantStatusExpired SupportGrantStatus = "expired"
	SupportGrantStatusDenied  SupportGrantStatus = "denied"
)

// SupportCaseStatus represents the support case lifecycle.
type SupportCaseStatus string

const (
	SupportCaseStatusOpen            SupportCaseStatus = "open"
	SupportCaseStatusInvestigating   SupportCaseStatus = "investigating"
	SupportCaseStatusWaiting         SupportCaseStatus = "waiting"
	SupportCaseStatusWaitingCustomer SupportCaseStatus = "waiting_customer"
	SupportCaseStatusWaitingInternal SupportCaseStatus = "waiting_internal"
	SupportCaseStatusResolved        SupportCaseStatus = "resolved"
	SupportCaseStatusClosed          SupportCaseStatus = "closed"
)

// RecoveryCheckpointStatus represents the lifecycle of a verified recovery point.
type RecoveryCheckpointStatus string

const (
	RecoveryCheckpointStatusPending   RecoveryCheckpointStatus = "pending"
	RecoveryCheckpointStatusCreating  RecoveryCheckpointStatus = "creating"
	RecoveryCheckpointStatusReady     RecoveryCheckpointStatus = "ready"
	RecoveryCheckpointStatusRestoring RecoveryCheckpointStatus = "restoring"
	RecoveryCheckpointStatusRestored  RecoveryCheckpointStatus = "restored"
	RecoveryCheckpointStatusFailed    RecoveryCheckpointStatus = "failed"
	RecoveryCheckpointStatusExpired   RecoveryCheckpointStatus = "expired"
	RecoveryCheckpointStatusDeleted   RecoveryCheckpointStatus = "deleted"
)

// SupportAccessGrant authorizes one support user for explicit scopes and a
// short time window. Access tokens and network restrictions remain server-only.
type SupportAccessGrant struct {
	BaseModel
	OrganizationID   uuid.UUID          `gorm:"type:uuid;not null;index" json:"organization_id"`
	SupportCaseID    *uuid.UUID         `gorm:"type:uuid;index" json:"support_case_id,omitempty"`
	ResellerID       *uuid.UUID         `gorm:"type:uuid;index" json:"reseller_id,omitempty"`
	GranteeUserID    uuid.UUID          `gorm:"type:uuid;not null;index" json:"grantee_user_id"`
	RequestedByID    uuid.UUID          `gorm:"type:uuid;not null;index" json:"requested_by_id"`
	ApprovedByID     *uuid.UUID         `gorm:"type:uuid;index" json:"approved_by_id,omitempty"`
	Status           SupportGrantStatus `gorm:"size:20;not null;default:'pending';index" json:"status"`
	Scopes           StringArray        `gorm:"type:jsonb;not null;default:'[]'" json:"scopes"`
	Purpose          string             `gorm:"type:text;not null" json:"purpose"`
	AccessTokenHash  string             `gorm:"size:255" json:"-"`
	IPAllowlist      StringArray        `gorm:"type:jsonb;not null;default:'[]'" json:"-"`
	RequireMFA       bool               `gorm:"not null;default:true" json:"require_mfa"`
	MaxSessions      int                `gorm:"not null;default:1" json:"max_sessions"`
	RequestedAt      time.Time          `gorm:"not null;index" json:"requested_at"`
	ApprovedAt       *time.Time         `json:"approved_at,omitempty"`
	ExpiresAt        time.Time          `gorm:"not null;index" json:"expires_at"`
	LastAccessedAt   *time.Time         `json:"last_accessed_at,omitempty"`
	RevokedByID      *uuid.UUID         `gorm:"type:uuid;index" json:"revoked_by_id,omitempty"`
	RevokedAt        *time.Time         `json:"revoked_at,omitempty"`
	RevocationReason string             `gorm:"type:text" json:"revocation_reason,omitempty"`
}

func (SupportAccessGrant) TableName() string {
	return "support_access_grants"
}

// SupportCase represents an organization-scoped operational support request.
type SupportCase struct {
	BaseModel
	OrganizationID      uuid.UUID         `gorm:"type:uuid;not null;index" json:"organization_id"`
	ResellerID          *uuid.UUID        `gorm:"type:uuid;index" json:"reseller_id,omitempty"`
	CaseNumber          string            `gorm:"size:100;not null;uniqueIndex" json:"case_number"`
	Status              SupportCaseStatus `gorm:"size:30;not null;default:'open';index" json:"status"`
	Priority            string            `gorm:"size:20;not null;default:'normal';index" json:"priority"`
	Category            string            `gorm:"size:100;not null;index" json:"category"`
	Subject             string            `gorm:"size:255;not null" json:"subject"`
	Description         string            `gorm:"type:text;not null" json:"description"`
	ReporterUserID      *uuid.UUID        `gorm:"type:uuid;index" json:"reporter_user_id,omitempty"`
	AssignedToID        *uuid.UUID        `gorm:"type:uuid;index" json:"assigned_to_id,omitempty"`
	ContactProfile      JSONB             `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	RelatedResourceType string            `gorm:"size:100" json:"related_resource_type,omitempty"`
	RelatedResourceID   string            `gorm:"size:255" json:"related_resource_id,omitempty"`
	Metadata            JSONB             `gorm:"type:jsonb;not null;default:'{}'" json:"metadata"`
	OpenedAt            time.Time         `gorm:"not null;index" json:"opened_at"`
	FirstResponseDueAt  *time.Time        `gorm:"index" json:"first_response_due_at,omitempty"`
	ResolutionDueAt     *time.Time        `gorm:"index" json:"resolution_due_at,omitempty"`
	FirstRespondedAt    *time.Time        `json:"first_responded_at,omitempty"`
	ResolvedAt          *time.Time        `json:"resolved_at,omitempty"`
	ClosedAt            *time.Time        `json:"closed_at,omitempty"`
	Resolution          string            `gorm:"type:text" json:"resolution,omitempty"`
}

func (SupportCase) TableName() string {
	return "support_cases"
}

// RecoveryCheckpoint is the metadata for an encrypted recovery artifact.
// Storage coordinates and key references must only be used by recovery services.
type RecoveryCheckpoint struct {
	BaseModel
	OrganizationID       uuid.UUID                `gorm:"type:uuid;not null;index" json:"organization_id"`
	SupportCaseID        *uuid.UUID               `gorm:"type:uuid;index" json:"support_case_id,omitempty"`
	Type                 string                   `gorm:"size:50;not null;index" json:"type"`
	Status               RecoveryCheckpointStatus `gorm:"size:30;not null;default:'pending';index" json:"status"`
	Reason               string                   `gorm:"type:text;not null" json:"reason"`
	Scope                JSONB                    `gorm:"type:jsonb;not null;default:'{}'" json:"-"`
	StorageProvider      string                   `gorm:"size:50" json:"-"`
	StorageBucket        string                   `gorm:"size:255" json:"-"`
	StorageObjectKey     string                   `gorm:"type:text" json:"-"`
	EncryptionKeyRef     string                   `gorm:"size:255" json:"-"`
	Checksum             string                   `gorm:"size:128" json:"checksum,omitempty"`
	SizeBytes            int64                    `gorm:"not null;default:0" json:"size_bytes"`
	DatabasePointInTime  *time.Time               `json:"database_point_in_time,omitempty"`
	CreatedByID          *uuid.UUID               `gorm:"type:uuid;index" json:"created_by_id,omitempty"`
	VerifiedByID         *uuid.UUID               `gorm:"type:uuid;index" json:"verified_by_id,omitempty"`
	RestoreRequestedByID *uuid.UUID               `gorm:"type:uuid;index" json:"restore_requested_by_id,omitempty"`
	ReadyAt              *time.Time               `json:"ready_at,omitempty"`
	VerifiedAt           *time.Time               `json:"verified_at,omitempty"`
	ExpiresAt            *time.Time               `gorm:"index" json:"expires_at,omitempty"`
	RestoreStartedAt     *time.Time               `json:"restore_started_at,omitempty"`
	RestoredAt           *time.Time               `json:"restored_at,omitempty"`
	ErrorMessage         string                   `gorm:"type:text" json:"error_message,omitempty"`
	Metadata             JSONB                    `gorm:"type:jsonb;not null;default:'{}'" json:"metadata"`
}

func (RecoveryCheckpoint) TableName() string {
	return "recovery_checkpoints"
}
