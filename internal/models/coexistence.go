package models

import (
	"time"

	"github.com/google/uuid"
)

// CoexistenceOnboardingStatus is the durable Embedded Signup lifecycle for a
// WhatsApp Business App number connected to Cloud API in coexistence mode.
type CoexistenceOnboardingStatus string

const (
	// CoexistenceSyncWindow is Meta's deadline for requesting Business App
	// data synchronization after onboarding completes.
	CoexistenceSyncWindow = 24 * time.Hour

	CoexistenceOnboardingStatusPending    CoexistenceOnboardingStatus = "pending"
	CoexistenceOnboardingStatusConnected  CoexistenceOnboardingStatus = "connected"
	CoexistenceOnboardingStatusSyncing    CoexistenceOnboardingStatus = "syncing"
	CoexistenceOnboardingStatusReady      CoexistenceOnboardingStatus = "ready"
	CoexistenceOnboardingStatusFailed     CoexistenceOnboardingStatus = "failed"
	CoexistenceOnboardingStatusExpired    CoexistenceOnboardingStatus = "expired"
	CoexistenceOnboardingStatusOffboarded CoexistenceOnboardingStatus = "offboarded"
)

// CoexistenceSyncStatus records both aggregate and per-data-set progress. A
// request is marked requesting before the provider call so an interrupted or
// ambiguous attempt is distinguishable from a request that was never made.
type CoexistenceSyncStatus string

const (
	CoexistenceSyncStatusNotRequested CoexistenceSyncStatus = "not_requested"
	CoexistenceSyncStatusPending      CoexistenceSyncStatus = "pending"
	CoexistenceSyncStatusRequesting   CoexistenceSyncStatus = "requesting"
	CoexistenceSyncStatusRequested    CoexistenceSyncStatus = "requested"
	CoexistenceSyncStatusInProgress   CoexistenceSyncStatus = "in_progress"
	CoexistenceSyncStatusCompleted    CoexistenceSyncStatus = "completed"
	CoexistenceSyncStatusFailed       CoexistenceSyncStatus = "failed"
	CoexistenceSyncStatusDeclined     CoexistenceSyncStatus = "declined"
	CoexistenceSyncStatusExpired      CoexistenceSyncStatus = "expired"
)

// CoexistenceHistoryConsent captures the business user's explicit choice in
// Meta's onboarding flow. Unknown is intentionally distinct from declined.
type CoexistenceHistoryConsent string

const (
	CoexistenceHistoryConsentUnknown  CoexistenceHistoryConsent = "unknown"
	CoexistenceHistoryConsentGranted  CoexistenceHistoryConsent = "granted"
	CoexistenceHistoryConsentDeclined CoexistenceHistoryConsent = "declined"
)

// CoexistenceLifecycleStatus tracks whether the Business App/Cloud API link is
// usable independently of its initial contact and history synchronization.
type CoexistenceLifecycleStatus string

const (
	CoexistenceLifecycleStatusUnknown      CoexistenceLifecycleStatus = "unknown"
	CoexistenceLifecycleStatusConnected    CoexistenceLifecycleStatus = "connected"
	CoexistenceLifecycleStatusDisconnected CoexistenceLifecycleStatus = "disconnected"
	CoexistenceLifecycleStatusOffboarded   CoexistenceLifecycleStatus = "offboarded"
)

// WhatsAppCoexistenceState is the resumable, tenant-owned state for one
// WhatsApp account onboarded in Business App coexistence mode. It stores no
// provider credentials or webhook bodies. Provider request IDs are retained
// only for correlation and lifecycle metadata should remain non-secret.
type WhatsAppCoexistenceState struct {
	ID                uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	CreatedAt         time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt         time.Time `gorm:"autoUpdateTime" json:"updated_at"`
	OrganizationID    uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:idx_whatsapp_coexistence_org_account,priority:1" json:"organization_id"`
	WhatsAppAccountID uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:idx_whatsapp_coexistence_org_account,priority:2" json:"whatsapp_account_id"`
	// BusinessPhoneNumber is Meta's display_phone_number captured during
	// Embedded Signup. It lets WABA-scoped account_update events that include a
	// phone number target one Coexistence account without exposing the value in
	// account synchronization responses.
	BusinessPhoneNumber string `gorm:"size:50;index" json:"-"`

	OnboardingStatus       CoexistenceOnboardingStatus `gorm:"size:24;not null;default:'pending';index" json:"onboarding_status"`
	OnboardingErrorCode    string                      `gorm:"size:100" json:"-"`
	OnboardingErrorMessage string                      `gorm:"type:text" json:"-"`
	OnboardedAt            *time.Time                  `gorm:"index" json:"onboarded_at,omitempty"`
	// OnboardingCycle increments whenever an offboarded number completes a new
	// Embedded Signup. Coexistence history callbacks do not include the request
	// ID, so later cycles require a provider event timestamp before a callback
	// may advance their state; payload data is still persisted idempotently.
	OnboardingCycle uint64 `gorm:"not null;default:1" json:"-"`

	SyncStatus      CoexistenceSyncStatus `gorm:"size:24;not null;default:'not_requested';index" json:"sync_status"`
	SyncStartedAt   *time.Time            `json:"sync_started_at,omitempty"`
	SyncCompletedAt *time.Time            `json:"sync_completed_at,omitempty"`
	SyncDeadlineAt  *time.Time            `gorm:"index" json:"sync_deadline_at,omitempty"`

	ContactSyncStatus        CoexistenceSyncStatus `gorm:"size:24;not null;default:'not_requested';index" json:"contact_sync_status"`
	ContactSyncRequestID     string                `gorm:"size:255;index" json:"contact_sync_request_id,omitempty"`
	ContactSyncAttempts      int                   `gorm:"not null;default:0" json:"contact_sync_attempts"`
	ContactSyncLastAttemptAt *time.Time            `json:"contact_sync_last_attempt_at,omitempty"`
	ContactSyncRequestedAt   *time.Time            `json:"contact_sync_requested_at,omitempty"`
	ContactSyncCompletedAt   *time.Time            `json:"contact_sync_completed_at,omitempty"`
	ContactSyncErrorCode     string                `gorm:"size:100" json:"-"`
	ContactSyncErrorMessage  string                `gorm:"type:text" json:"-"`

	HistoryConsent           CoexistenceHistoryConsent `gorm:"size:16;not null;default:'unknown';index" json:"history_consent"`
	HistoryConsentAt         *time.Time                `json:"history_consent_at,omitempty"`
	HistorySyncStatus        CoexistenceSyncStatus     `gorm:"size:24;not null;default:'not_requested';index" json:"history_sync_status"`
	HistorySyncRequestID     string                    `gorm:"size:255;index" json:"history_sync_request_id,omitempty"`
	HistorySyncAttempts      int                       `gorm:"not null;default:0" json:"history_sync_attempts"`
	HistorySyncLastAttemptAt *time.Time                `json:"history_sync_last_attempt_at,omitempty"`
	HistorySyncRequestedAt   *time.Time                `json:"history_sync_requested_at,omitempty"`
	HistoryLastPhase         *int                      `json:"history_last_phase,omitempty"`
	HistoryLastChunkOrder    *int                      `json:"history_last_chunk_order,omitempty"`
	HistoryProgressPercent   int                       `gorm:"not null;default:0" json:"history_progress_percent"`
	HistoryProgressAt        *time.Time                `json:"history_progress_at,omitempty"`
	HistoryCompletedAt       *time.Time                `json:"history_completed_at,omitempty"`
	HistorySyncErrorCode     string                    `gorm:"size:100" json:"-"`
	HistorySyncErrorMessage  string                    `gorm:"type:text" json:"-"`

	LifecycleStatus         CoexistenceLifecycleStatus `gorm:"size:24;not null;default:'unknown';index" json:"lifecycle_status"`
	LastLifecycleEvent      string                     `gorm:"size:64;index" json:"last_lifecycle_event,omitempty"`
	LastLifecycleEventAt    *time.Time                 `gorm:"index" json:"last_lifecycle_event_at,omitempty"`
	LifecycleMetadata       JSONB                      `gorm:"type:jsonb;not null;default:'{}'" json:"lifecycle_metadata"`
	DisconnectedAt          *time.Time                 `json:"disconnected_at,omitempty"`
	OffboardedAt            *time.Time                 `json:"offboarded_at,omitempty"`
	ReconnectedAt           *time.Time                 `json:"reconnected_at,omitempty"`
	DisconnectReasonCode    string                     `gorm:"size:100" json:"disconnect_reason_code,omitempty"`
	DisconnectReasonMessage string                     `gorm:"type:text" json:"-"`

	// Version supports optimistic state-machine updates by callers. Increment
	// it in the same UPDATE that transitions a state.
	Version uint64 `gorm:"not null;default:1" json:"version"`

	Organization    *Organization    `gorm:"foreignKey:OrganizationID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"-"`
	WhatsAppAccount *WhatsAppAccount `gorm:"foreignKey:WhatsAppAccountID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE" json:"-"`
}

func (WhatsAppCoexistenceState) TableName() string {
	return "whatsapp_coexistence_states"
}

// WhatsAppIdentityReviewSelectorReason records every authenticated selector
// that found a physical contact. The values are a bit set so one canonical
// contact can retain every reason after merge-alias canonicalization.
type WhatsAppIdentityReviewSelectorReason uint8

const (
	WhatsAppIdentityReviewSelectorPrimaryBSUID WhatsAppIdentityReviewSelectorReason = 1
	WhatsAppIdentityReviewSelectorParentBSUID  WhatsAppIdentityReviewSelectorReason = 2
	WhatsAppIdentityReviewSelectorPhone        WhatsAppIdentityReviewSelectorReason = 4
	WhatsAppIdentityReviewSelectorReasonMask                                        = WhatsAppIdentityReviewSelectorPrimaryBSUID |
		WhatsAppIdentityReviewSelectorParentBSUID |
		WhatsAppIdentityReviewSelectorPhone
)

func (reason WhatsAppIdentityReviewSelectorReason) Valid() bool {
	return reason != 0 && reason&^WhatsAppIdentityReviewSelectorReasonMask == 0
}

func (reason WhatsAppIdentityReviewSelectorReason) Has(flag WhatsAppIdentityReviewSelectorReason) bool {
	return reason&flag == flag
}

// WhatsAppIdentityReviewDisposition is the write-once lifecycle of a review
// generation. Open rows block automation. Only the latest supported generation
// may receive a future-routing decision; older reviewed open generations are
// atomically closed with the non-routing superseded disposition.
type WhatsAppIdentityReviewDisposition string

const (
	WhatsAppIdentityReviewDispositionOpen               WhatsAppIdentityReviewDisposition = "open"
	WhatsAppIdentityReviewDispositionFutureRouting      WhatsAppIdentityReviewDisposition = "future_routing"
	WhatsAppIdentityReviewDispositionSupersededByLatest WhatsAppIdentityReviewDisposition = "superseded_by_latest"
	// SupersededByCycle is an audited, permanent, non-routing closure applied
	// atomically when an account advances to a later onboarding cycle. It can
	// never be interpreted as a reviewed future-routing decision.
	WhatsAppIdentityReviewDispositionSupersededByCycle WhatsAppIdentityReviewDisposition = "superseded_by_cycle"
)

const WhatsAppIdentityReviewProtocolVersion uint16 = 1

// WhatsAppIdentityReviewHold is immutable claim and membership authority plus
// one permitted write-once decision transition. It deliberately contains no
// soft-delete column: review history cannot disappear when an account, contact,
// or transfer changes. Normalized selector values and event provenance are
// private storage fields and must never be serialized directly to clients.
type WhatsAppIdentityReviewHold struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid();uniqueIndex:uq_whatsapp_identity_review_hold_org_id,priority:2" json:"id"`
	CreatedAt time.Time `gorm:"autoCreateTime" json:"created_at"`
	UpdatedAt time.Time `gorm:"autoUpdateTime" json:"updated_at"`

	OrganizationID    uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:uq_whatsapp_identity_review_hold_org_id,priority:1" json:"organization_id"`
	WhatsAppAccountID uuid.UUID `gorm:"type:uuid;not null;index" json:"whatsapp_account_id"`
	OnboardingCycle   uint64    `gorm:"not null" json:"onboarding_cycle"`
	ProtocolVersion   uint16    `gorm:"not null" json:"protocol_version"`

	// Supported is true only when an authenticated, non-empty direct primary
	// BSUID establishes the immutable routing principal. Unsupported holds stay
	// open blockers and can never become a routing decision.
	Supported           bool   `gorm:"not null;default:false;index" json:"supported"`
	DirectPrimaryBSUID  string `gorm:"column:direct_primary_bsuid;size:255;not null;default:'';index" json:"-"`
	ParentBSUID         string `gorm:"column:parent_bsuid;size:255;not null;default:''" json:"-"`
	Phone               string `gorm:"size:50;not null;default:''" json:"-"`
	PrincipalGeneration uint64 `gorm:"not null;default:0" json:"principal_generation"`

	SemanticClaimDigest     string `gorm:"size:64;not null;index" json:"semantic_claim_digest"`
	SelectorBodyDigest      string `gorm:"size:64;not null" json:"-"`
	VerifiedEventDigest     string `gorm:"size:64;not null" json:"-"`
	VerifiedEventProvenance string `gorm:"size:64;not null" json:"-"`

	MemberCount  uint32                            `gorm:"not null" json:"member_count"`
	MemberDigest string                            `gorm:"size:64;not null" json:"member_digest"`
	Version      uint64                            `gorm:"not null;default:1" json:"version"`
	Disposition  WhatsAppIdentityReviewDisposition `gorm:"size:32;not null;default:'open';index" json:"disposition"`

	DecisionTargetContactID     *uuid.UUID `gorm:"type:uuid;index" json:"decision_target_contact_id,omitempty"`
	DecisionResolvedByID        *uuid.UUID `gorm:"type:uuid;index" json:"decision_resolved_by_id,omitempty"`
	DecisionResolvedAt          *time.Time `json:"decision_resolved_at,omitempty"`
	DecisionRequestID           *uuid.UUID `gorm:"type:uuid;index" json:"decision_request_id,omitempty"`
	DecisionRequestDigest       string     `gorm:"size:64;not null;default:''" json:"decision_request_digest,omitempty"`
	DecisionChainDigest         string     `gorm:"size:64;not null;default:''" json:"decision_chain_digest,omitempty"`
	SupersededByHoldID          *uuid.UUID `gorm:"type:uuid;index" json:"superseded_by_hold_id,omitempty"`
	SupersededByGeneration      *uint64    `json:"superseded_by_generation,omitempty"`
	CycleSupersededAt           *time.Time `json:"cycle_superseded_at,omitempty"`
	SupersededByOnboardingCycle *uint64    `json:"superseded_by_onboarding_cycle,omitempty"`

	Organization     *Organization                  `gorm:"foreignKey:OrganizationID;-:migration" json:"-"`
	WhatsAppAccount  *WhatsAppAccount               `gorm:"foreignKey:WhatsAppAccountID;-:migration" json:"-"`
	Members          []WhatsAppIdentityReviewMember `gorm:"foreignKey:OrganizationID,HoldID;references:OrganizationID,ID;-:migration" json:"members,omitempty"`
	DecisionTarget   *Contact                       `gorm:"foreignKey:DecisionTargetContactID;-:migration" json:"-"`
	DecisionResolver *User                          `gorm:"foreignKey:DecisionResolvedByID;-:migration" json:"-"`
}

func (WhatsAppIdentityReviewHold) TableName() string {
	return "whatsapp_identity_review_holds"
}

// WhatsAppIdentityReviewMember is the exact immutable candidate set captured
// with a hold. The tenant/hold/contact triple is the key; there is no surrogate
// identifier, soft delete, or mutable timestamp.
type WhatsAppIdentityReviewMember struct {
	OrganizationID  uuid.UUID                            `gorm:"type:uuid;primaryKey;index:idx_whatsapp_identity_review_member_contact,priority:1" json:"organization_id"`
	HoldID          uuid.UUID                            `gorm:"type:uuid;primaryKey;index:idx_whatsapp_identity_review_member_contact,priority:3" json:"hold_id"`
	ContactID       uuid.UUID                            `gorm:"type:uuid;primaryKey;index:idx_whatsapp_identity_review_member_contact,priority:2" json:"contact_id"`
	SelectorReasons WhatsAppIdentityReviewSelectorReason `gorm:"not null" json:"selector_reasons"`
	CreatedAt       time.Time                            `gorm:"autoCreateTime" json:"created_at"`

	Hold    *WhatsAppIdentityReviewHold `gorm:"foreignKey:OrganizationID,HoldID;references:OrganizationID,ID;-:migration" json:"-"`
	Contact *Contact                    `gorm:"foreignKey:OrganizationID,ContactID;references:OrganizationID,ID;-:migration" json:"-"`
}

func (WhatsAppIdentityReviewMember) TableName() string {
	return "whatsapp_identity_review_members"
}
