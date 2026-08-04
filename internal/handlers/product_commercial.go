package handlers

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/audit"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	productPlanAuditResource           = "commercial_plan"
	productOnboardingAuditResource     = "organization_onboarding"
	productTemplateAuditResource       = "workspace_template_application"
	productRetentionAuditResource      = "retention_policy"
	productConsentAuditResource        = "consent_event"
	productPrivacyRequestAuditResource = "privacy_request"
	productSupportCaseAuditResource    = "support_case"
	productSubscriptionAuditResource   = "commercial_subscription"

	productCommercialMaxProfileBytes  = 32 * 1024
	productCommercialMaxEvidenceBytes = 16 * 1024
)

// ProductPlanPriceResponse is the public, provider-neutral price shape.
type ProductPlanPriceResponse struct {
	ID               *uuid.UUID             `json:"id,omitempty"`
	Code             string                 `json:"code"`
	Currency         string                 `json:"currency"`
	UnitAmountMinor  int64                  `json:"unit_amount_minor"`
	SetupAmountMinor int64                  `json:"setup_amount_minor"`
	Interval         models.BillingInterval `json:"interval"`
	IntervalCount    int                    `json:"interval_count"`
	TaxBehavior      string                 `json:"tax_behavior"`
	Assignable       bool                   `json:"assignable"`
}

// ProductPlanResponse deliberately excludes provider IDs and provider payloads.
type ProductPlanResponse struct {
	ID           *uuid.UUID                  `json:"id,omitempty"`
	Code         string                      `json:"code"`
	Name         string                      `json:"name"`
	Description  string                      `json:"description,omitempty"`
	Vertical     string                      `json:"vertical"`
	Status       models.CommercialPlanStatus `json:"status"`
	TrialDays    int                         `json:"trial_days"`
	IsPublic     bool                        `json:"is_public"`
	Entitlements map[string]any              `json:"entitlements"`
	Prices       []ProductPlanPriceResponse  `json:"prices"`
}

// ProductSubscriptionResponse is the safe subscription summary used by the UI.
type ProductSubscriptionResponse struct {
	ID                 *uuid.UUID             `json:"id,omitempty"`
	PlanID             *uuid.UUID             `json:"plan_id,omitempty"`
	PlanPriceID        *uuid.UUID             `json:"plan_price_id,omitempty"`
	PlanCode           string                 `json:"plan_code"`
	PlanName           string                 `json:"plan_name,omitempty"`
	Status             string                 `json:"status"`
	Provider           models.BillingProvider `json:"provider"`
	TrialEndsAt        *time.Time             `json:"trial_ends_at,omitempty"`
	GraceUntil         *time.Time             `json:"grace_until,omitempty"`
	CurrentPeriodStart *time.Time             `json:"current_period_start,omitempty"`
	CurrentPeriodEnd   *time.Time             `json:"current_period_end,omitempty"`
	CancelAtPeriodEnd  bool                   `json:"cancel_at_period_end"`
	CancelAt           *time.Time             `json:"cancel_at,omitempty"`
}

// ProductEntitlementsResponse is the effective entitlement set after overrides.
type ProductEntitlementsResponse struct {
	PlanCode           string                    `json:"plan_code"`
	Mode               string                    `json:"mode"`
	SubscriptionStatus models.SubscriptionStatus `json:"subscription_status,omitempty"`
	Entitlements       map[string]any            `json:"entitlements"`
	OverriddenKeys     []string                  `json:"overridden_keys"`
	EvaluatedAt        time.Time                 `json:"evaluated_at"`
}

// ProductEntitlementDecision is the server-side licensing result for one
// feature key. It contains no billing-provider identifiers or payloads.
type ProductEntitlementDecision struct {
	Key                string                    `json:"key"`
	Allowed            bool                      `json:"allowed"`
	Value              any                       `json:"value,omitempty"`
	Mode               string                    `json:"mode"`
	SubscriptionID     *uuid.UUID                `json:"subscription_id,omitempty"`
	SubscriptionStatus models.SubscriptionStatus `json:"subscription_status,omitempty"`
	Overridden         bool                      `json:"overridden"`
	Reason             string                    `json:"reason"`
}

// PlanPriceInput creates an immutable provider-neutral price.
type PlanPriceInput struct {
	Code             string                 `json:"code"`
	Currency         string                 `json:"currency"`
	UnitAmountMinor  int64                  `json:"unit_amount_minor"`
	SetupAmountMinor int64                  `json:"setup_amount_minor,omitempty"`
	Interval         models.BillingInterval `json:"interval"`
	IntervalCount    int                    `json:"interval_count,omitempty"`
	TaxBehavior      string                 `json:"tax_behavior,omitempty"`
}

// PlanEntitlementInput creates or replaces one normalized entitlement value.
type PlanEntitlementInput struct {
	Key                    string                        `json:"key"`
	ValueType              models.EntitlementValueType   `json:"value_type"`
	Value                  any                           `json:"value"`
	Enforcement            models.EntitlementEnforcement `json:"enforcement,omitempty"`
	ResetInterval          models.BillingInterval        `json:"reset_interval,omitempty"`
	OverageUnitAmountMinor int64                         `json:"overage_unit_amount_minor,omitempty"`
	OverageCurrency        string                        `json:"overage_currency,omitempty"`
	Description            string                        `json:"description,omitempty"`
}

// CreateProductPlanRequest creates a platform or reseller plan and initial catalog.
type CreateProductPlanRequest struct {
	ResellerID   *uuid.UUID                  `json:"reseller_id,omitempty"`
	Code         string                      `json:"code"`
	Name         string                      `json:"name"`
	Description  string                      `json:"description,omitempty"`
	Vertical     string                      `json:"vertical,omitempty"`
	Status       models.CommercialPlanStatus `json:"status,omitempty"`
	TrialDays    int                         `json:"trial_days,omitempty"`
	DisplayOrder int                         `json:"display_order,omitempty"`
	IsPublic     bool                        `json:"is_public"`
	Prices       []PlanPriceInput            `json:"prices"`
	Entitlements []PlanEntitlementInput      `json:"entitlements,omitempty"`
}

// UpdateProductPlanRequest keeps published prices immutable. New price codes
// may be added and existing codes may be deactivated.
type UpdateProductPlanRequest struct {
	Name                 *string                      `json:"name,omitempty"`
	Description          *string                      `json:"description,omitempty"`
	Vertical             *string                      `json:"vertical,omitempty"`
	Status               *models.CommercialPlanStatus `json:"status,omitempty"`
	TrialDays            *int                         `json:"trial_days,omitempty"`
	DisplayOrder         *int                         `json:"display_order,omitempty"`
	IsPublic             *bool                        `json:"is_public,omitempty"`
	NewPrices            []PlanPriceInput             `json:"new_prices,omitempty"`
	DeactivatePriceCodes []string                     `json:"deactivate_price_codes,omitempty"`
	Entitlements         []PlanEntitlementInput       `json:"entitlements,omitempty"`
}

// SetOrganizationSubscriptionRequest is intentionally manual-only. It accepts
// no payment success, provider event, invoice, or paid-at fields.
type SetOrganizationSubscriptionRequest struct {
	PlanID          *uuid.UUID                `json:"plan_id,omitempty"`
	PlanCode        string                    `json:"plan_code,omitempty"`
	PlanPriceID     *uuid.UUID                `json:"plan_price_id,omitempty"`
	PriceCode       string                    `json:"price_code,omitempty"`
	Status          models.SubscriptionStatus `json:"status"`
	TrialDays       *int                      `json:"trial_days,omitempty"`
	ManualReference string                    `json:"manual_reference"`
}

// OnboardingStepResponse is one resumable go-live milestone.
type OnboardingStepResponse struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Completed   bool   `json:"completed"`
	Inferred    bool   `json:"inferred"`
	ActionPath  string `json:"action_path,omitempty"`
}

// OnboardingResponse is a safe projection of OrganizationOnboarding.
type OnboardingResponse struct {
	ID                *uuid.UUID               `json:"id,omitempty"`
	Vertical          string                   `json:"vertical"`
	Status            models.OnboardingStatus  `json:"status"`
	ProvisioningState string                   `json:"provisioning_state"`
	CurrentStep       string                   `json:"current_step,omitempty"`
	ProgressPercent   int                      `json:"progress_percent"`
	Steps             []OnboardingStepResponse `json:"steps"`
	Profile           models.JSONB             `json:"profile"`
}

// UpdateOnboardingProfileRequest replaces the business profile used by provisioning.
type UpdateOnboardingProfileRequest struct {
	Profile models.JSONB `json:"profile"`
}

// WorkspaceTemplateResponse is the public catalog representation.
type WorkspaceTemplateResponse struct {
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Vertical    string   `json:"vertical"`
	Description string   `json:"description"`
	Version     int      `json:"version"`
	Highlights  []string `json:"highlights"`
}

// ApplyWorkspaceTemplateResponse makes retries observable to clients.
type ApplyWorkspaceTemplateResponse struct {
	ApplicationID uuid.UUID `json:"application_id"`
	TemplateKey   string    `json:"template_key"`
	Version       int       `json:"version"`
	Status        string    `json:"status"`
	Idempotent    bool      `json:"idempotent"`
}

// RetentionPolicyInput is accepted by UpdatePrivacySettings.
type RetentionPolicyInput struct {
	DataCategory    string                 `json:"data_category"`
	RetentionDays   int                    `json:"retention_days"`
	GracePeriodDays int                    `json:"grace_period_days,omitempty"`
	Action          models.RetentionAction `json:"action"`
	LegalBasis      string                 `json:"legal_basis,omitempty"`
	IsEnabled       *bool                  `json:"is_enabled,omitempty"`
}

// UpdatePrivacySettingsRequest upserts only the supplied policy categories.
type UpdatePrivacySettingsRequest struct {
	RetentionPolicies []RetentionPolicyInput `json:"retention_policies"`
}

// RetentionPolicyResponse is compatible with the trust-center client contract.
type RetentionPolicyResponse struct {
	ID              uuid.UUID              `json:"id"`
	DataCategory    string                 `json:"data_category"`
	RetentionDays   int                    `json:"retention_days"`
	GracePeriodDays int                    `json:"grace_period_days"`
	Action          models.RetentionAction `json:"action"`
	LegalBasis      string                 `json:"legal_basis,omitempty"`
	IsEnabled       bool                   `json:"is_enabled"`
	EffectiveFrom   time.Time              `json:"effective_from"`
}

// RecordConsentRequest captures an immutable consent event and resulting state.
type RecordConsentRequest struct {
	ContactID     *uuid.UUID           `json:"contact_id,omitempty"`
	SubjectType   string               `json:"subject_type,omitempty"`
	SubjectKey    string               `json:"subject_key,omitempty"`
	Purpose       string               `json:"purpose"`
	Channel       string               `json:"channel"`
	Action        models.ConsentAction `json:"action"`
	LegalBasis    string               `json:"legal_basis,omitempty"`
	PolicyVersion string               `json:"policy_version,omitempty"`
	Source        string               `json:"source,omitempty"`
	Evidence      models.JSONB         `json:"evidence,omitempty"`
	ExpiresAt     *time.Time           `json:"expires_at,omitempty"`
}

// ConsentStateResponse excludes raw evidence while retaining provenance.
type ConsentStateResponse struct {
	ID            uuid.UUID            `json:"id"`
	ContactID     *uuid.UUID           `json:"contact_id,omitempty"`
	SubjectType   string               `json:"subject_type"`
	SubjectKey    string               `json:"subject_key"`
	Purpose       string               `json:"purpose"`
	Channel       string               `json:"channel"`
	Status        models.ConsentStatus `json:"status"`
	LatestEventID uuid.UUID            `json:"latest_event_id"`
	PolicyVersion string               `json:"policy_version,omitempty"`
	EffectiveAt   time.Time            `json:"effective_at"`
	ExpiresAt     *time.Time           `json:"expires_at,omitempty"`
}

// CreatePrivacyRequestRequest opens an identity-verification-first workflow.
type CreatePrivacyRequestRequest struct {
	RequestType      models.PrivacyRequestType `json:"request_type"`
	ContactID        *uuid.UUID                `json:"contact_id,omitempty"`
	SubjectType      string                    `json:"subject_type,omitempty"`
	SubjectKey       string                    `json:"subject_key,omitempty"`
	ReceivedChannel  string                    `json:"received_channel,omitempty"`
	RequesterProfile models.JSONB              `json:"requester_profile,omitempty"`
	Details          models.JSONB              `json:"details,omitempty"`
}

// UpdatePrivacyRequestRequest supports explicit, validated workflow transitions.
type UpdatePrivacyRequestRequest struct {
	Status         *models.PrivacyRequestStatus `json:"status,omitempty"`
	AssignedUserID *uuid.UUID                   `json:"assigned_user_id,omitempty"`
	ClearAssignee  bool                         `json:"clear_assignee,omitempty"`
	Resolution     *string                      `json:"resolution,omitempty"`
}

// PrivacyRequestResponse is the privacy desk's non-sensitive API shape.
type PrivacyRequestResponse struct {
	ID                 uuid.UUID                   `json:"id"`
	RequestNumber      string                      `json:"request_number"`
	ContactID          *uuid.UUID                  `json:"contact_id,omitempty"`
	RequestType        models.PrivacyRequestType   `json:"request_type"`
	Status             models.PrivacyRequestStatus `json:"status"`
	VerificationStatus string                      `json:"verification_status"`
	DueAt              time.Time                   `json:"due_at"`
	AssignedUserID     *uuid.UUID                  `json:"assigned_user_id,omitempty"`
	Resolution         string                      `json:"resolution,omitempty"`
	CreatedAt          time.Time                   `json:"created_at"`
	UpdatedAt          time.Time                   `json:"updated_at"`
}

// TenantHealthCheckResponse is one read-only operational signal.
type TenantHealthCheckResponse struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// TenantSupportHealthResponse contains no credentials or provider payloads.
type TenantSupportHealthResponse struct {
	Status    string                      `json:"status"`
	Checks    []TenantHealthCheckResponse `json:"checks"`
	CheckedAt time.Time                   `json:"checked_at"`
}

type TrustQueueSummaryResponse struct {
	PrivacyVisible bool  `json:"privacy_visible"`
	PrivacyOpen    int64 `json:"privacy_open"`
	SupportVisible bool  `json:"support_visible"`
	SupportOpen    int64 `json:"support_open"`
}

// CreateSupportCaseRequest opens a tenant-bound support case without granting access.
type CreateSupportCaseRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Severity    string `json:"severity,omitempty"`
	Category    string `json:"category,omitempty"`
}

// UpdateSupportCaseRequest supports controlled case changes.
type UpdateSupportCaseRequest struct {
	Title          *string                   `json:"title,omitempty"`
	Description    *string                   `json:"description,omitempty"`
	Severity       *string                   `json:"severity,omitempty"`
	Status         *models.SupportCaseStatus `json:"status,omitempty"`
	AssignedUserID *uuid.UUID                `json:"assigned_user_id,omitempty"`
	ClearAssignee  bool                      `json:"clear_assignee,omitempty"`
	Resolution     *string                   `json:"resolution,omitempty"`
}

// SupportCaseResponse matches the trust-center contract.
type SupportCaseResponse struct {
	ID             uuid.UUID                `json:"id"`
	CaseNumber     string                   `json:"case_number"`
	Title          string                   `json:"title"`
	Description    string                   `json:"description"`
	Severity       string                   `json:"severity"`
	Category       string                   `json:"category"`
	Status         models.SupportCaseStatus `json:"status"`
	AssignedUserID *uuid.UUID               `json:"assigned_user_id,omitempty"`
	Resolution     string                   `json:"resolution,omitempty"`
	CreatedAt      time.Time                `json:"created_at"`
	UpdatedAt      time.Time                `json:"updated_at"`
}

// RecoveryCheckpointResponse never contains object-store or encryption metadata.
type RecoveryCheckpointResponse struct {
	ID         uuid.UUID                       `json:"id"`
	Kind       string                          `json:"kind"`
	Status     models.RecoveryCheckpointStatus `json:"status"`
	CreatedAt  time.Time                       `json:"created_at"`
	ReadyAt    *time.Time                      `json:"ready_at,omitempty"`
	VerifiedAt *time.Time                      `json:"verified_at,omitempty"`
	ExpiresAt  *time.Time                      `json:"expires_at,omitempty"`
	SizeBytes  int64                           `json:"size_bytes"`
}

// RecoverySummaryResponse is a non-sensitive readiness view.
type RecoverySummaryResponse struct {
	Checkpoints    []RecoveryCheckpointResponse `json:"checkpoints"`
	LastVerifiedAt *time.Time                   `json:"last_verified_at,omitempty"`
	RecoveryReady  bool                         `json:"recovery_ready"`
}

type productCommercialClientError struct {
	status  int
	message string
}

func (e *productCommercialClientError) Error() string {
	return e.message
}

func newProductCommercialClientError(status int, message string) error {
	return &productCommercialClientError{status: status, message: message}
}

type builtInWorkspaceTemplate struct {
	Key         string
	Name        string
	Vertical    string
	Description string
	Highlights  []string
	Manifest    models.JSONB
	Pipeline    productTemplatePipelineSeed
	Services    []productTemplateServiceSeed
}

var productBuiltInWorkspaceTemplates = productCommercialBuildBuiltInTemplates()

var productCommercialIntendedChannelOrder = []models.Channel{
	models.ChannelWhatsApp,
	models.ChannelInstagram,
	models.ChannelMessenger,
	models.ChannelThreads,
	models.ChannelEmail,
	models.ChannelWebChat,
}

type productOnboardingStepDefinition struct {
	key         string
	label       string
	description string
	inferred    bool
	actionPath  string
}

var productOnboardingStepDefinitions = []productOnboardingStepDefinition{
	{
		key:         "workspace_profile",
		label:       "Confirm your workspace profile",
		description: "Add the business identity, operating timezone and channels required at launch.",
		actionPath:  "/launchpad#workspace-profile",
	},
	{
		key:         "license_assigned",
		label:       "Assign the workspace license",
		description: "Activate the intended commercial plan for this exact workspace.",
		inferred:    true,
		actionPath:  "/upgrade-workspace",
	},
	{
		key:         "vertical_template",
		label:       "Apply a vertical playbook",
		description: "Choose Clinic, Pharmacy or Wellness defaults.",
		inferred:    true,
	},
	{
		key:         "channel_connected",
		label:       "Connect every intended customer channel",
		description: "Authorize and test every channel selected in the workspace profile.",
		inferred:    true,
		actionPath:  "/inbox",
	},
	{
		key:         "team_invited",
		label:       "Invite the operating team",
		description: "Add at least one teammate beyond the workspace owner.",
		inferred:    true,
		actionPath:  "/settings/users",
	},
	{
		key:         "privacy_baseline",
		label:       "Set the privacy baseline",
		description: "Document at least one active retention rule.",
		inferred:    true,
		actionPath:  "/settings/privacy",
	},
	{
		key:         "go_live",
		label:       "Approve go-live",
		description: "Confirm the workspace is ready for customer traffic.",
	},
}

func (a *App) sendProductCommercialError(r *fastglue.Request, operation string, err error) error {
	var clientErr *productCommercialClientError
	if errors.As(err, &clientErr) {
		return r.SendErrorEnvelope(clientErr.status, clientErr.message, nil, "")
	}
	a.Log.Error("Product commercial operation failed", "operation", operation, "error", err)
	return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to "+operation, nil, "")
}

func productCommercialEntitlementValue(value models.JSONB) any {
	if len(value) == 1 {
		if scalar, ok := value["value"]; ok {
			return scalar
		}
	}
	return value
}

func productCommercialJSONCopy(value models.JSONB) models.JSONB {
	if value == nil {
		return models.JSONB{}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return models.JSONB{}
	}
	var copied models.JSONB
	if err := json.Unmarshal(data, &copied); err != nil {
		return models.JSONB{}
	}
	return copied
}

func productCommercialJSONBool(value models.JSONB, key string) bool {
	if value == nil {
		return false
	}
	result, _ := value[key].(bool)
	return result
}

func productCommercialString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func productCommercialStringSlice(value any) []string {
	raw, ok := value.([]any)
	if !ok {
		if typed, ok := value.([]string); ok {
			return append([]string(nil), typed...)
		}
		return []string{}
	}
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
			result = append(result, strings.TrimSpace(text))
		}
	}
	return result
}

func productCommercialBuiltInTemplate(key string) (builtInWorkspaceTemplate, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, template := range productBuiltInWorkspaceTemplates {
		if template.Key == key {
			return template, true
		}
	}
	return builtInWorkspaceTemplate{}, false
}

func productCommercialTemplateToResponse(
	template models.WorkspaceTemplate,
	version models.WorkspaceTemplateVersion,
) WorkspaceTemplateResponse {
	highlights := productCommercialStringSlice(version.Manifest["highlights"])
	return WorkspaceTemplateResponse{
		Key:         template.Slug,
		Name:        template.Name,
		Vertical:    template.Vertical,
		Description: template.Description,
		Version:     version.Version,
		Highlights:  highlights,
	}
}

func productCommercialBuiltinToResponse(template builtInWorkspaceTemplate) WorkspaceTemplateResponse {
	return WorkspaceTemplateResponse{
		Key:         template.Key,
		Name:        template.Name,
		Vertical:    template.Vertical,
		Description: template.Description,
		Version:     1,
		Highlights:  append([]string(nil), template.Highlights...),
	}
}

func productCommercialEffectiveSubscriptionStatus(
	subscription *models.Subscription,
	now time.Time,
) models.SubscriptionStatus {
	if subscription == nil {
		return ""
	}
	if productCommercialSubscriptionPermitsFeatures(subscription, now) {
		return subscription.Status
	}
	switch subscription.Status {
	case models.SubscriptionStatusActive,
		models.SubscriptionStatusTrialing,
		models.SubscriptionStatusPastDue:
		return models.SubscriptionStatusExpired
	default:
		return subscription.Status
	}
}

func productCommercialSubscriptionToResponseAt(
	subscription *models.Subscription,
	plan *models.Plan,
	now time.Time,
) ProductSubscriptionResponse {
	if subscription == nil {
		return ProductSubscriptionResponse{
			PlanCode: "unlicensed",
			PlanName: "No active plan",
			Status:   "unlicensed",
			Provider: models.BillingProviderManual,
		}
	}
	planID := subscription.PlanID
	response := ProductSubscriptionResponse{
		ID:                 &subscription.ID,
		PlanID:             &planID,
		PlanPriceID:        subscription.PlanPriceID,
		Status:             string(productCommercialEffectiveSubscriptionStatus(subscription, now)),
		Provider:           subscription.Provider,
		TrialEndsAt:        subscription.TrialEndsAt,
		GraceUntil:         subscription.GraceUntil,
		CurrentPeriodStart: subscription.CurrentPeriodStart,
		CurrentPeriodEnd:   subscription.CurrentPeriodEnd,
		CancelAtPeriodEnd:  subscription.CancelAtPeriodEnd,
		CancelAt:           subscription.CancelAt,
	}
	if plan != nil {
		response.PlanCode = plan.Code
		response.PlanName = plan.Name
	}
	if response.PlanCode == "" {
		response.PlanCode = "unavailable"
	}
	return response
}

func productCommercialSubscriptionToResponse(
	subscription *models.Subscription,
	plan *models.Plan,
) ProductSubscriptionResponse {
	return productCommercialSubscriptionToResponseAt(
		subscription,
		plan,
		time.Now().UTC(),
	)
}

type productCommercialSubscriptionAuditSnapshot struct {
	ProductSubscriptionResponse
	ManualReference string `json:"manual_reference,omitempty"`
}

func productCommercialAuditManualReference(providerData models.JSONB) string {
	reference := productCommercialString(providerData["manual_reference"])
	reference = strings.Map(func(value rune) rune {
		if unicode.IsControl(value) {
			return -1
		}
		return value
	}, reference)
	runes := []rune(reference)
	if len(runes) > 255 {
		reference = string(runes[:255])
	}
	return reference
}

func productCommercialSubscriptionToAuditSnapshotAt(
	subscription *models.Subscription,
	plan *models.Plan,
	now time.Time,
) productCommercialSubscriptionAuditSnapshot {
	snapshot := productCommercialSubscriptionAuditSnapshot{
		ProductSubscriptionResponse: productCommercialSubscriptionToResponseAt(
			subscription,
			plan,
			now,
		),
	}
	if subscription != nil {
		snapshot.ManualReference = productCommercialAuditManualReference(
			subscription.ProviderData,
		)
	}
	return snapshot
}

func productCommercialPrivacyRequestToResponse(request models.PrivacyRequest) PrivacyRequestResponse {
	verificationStatus := "pending"
	if request.VerifiedAt != nil {
		verificationStatus = "verified"
	} else if request.Status == models.PrivacyRequestStatusDenied ||
		request.Status == models.PrivacyRequestStatusCanceled ||
		request.Status == models.PrivacyRequestStatusExpired {
		verificationStatus = "closed"
	}
	return PrivacyRequestResponse{
		ID:                 request.ID,
		RequestNumber:      request.RequestNumber,
		ContactID:          request.ContactID,
		RequestType:        request.Type,
		Status:             request.Status,
		VerificationStatus: verificationStatus,
		DueAt:              request.DueAt,
		AssignedUserID:     request.AssignedToID,
		Resolution:         request.DecisionReason,
		CreatedAt:          request.CreatedAt,
		UpdatedAt:          request.UpdatedAt,
	}
}

func productCommercialSupportCaseToResponse(supportCase models.SupportCase) SupportCaseResponse {
	return SupportCaseResponse{
		ID:             supportCase.ID,
		CaseNumber:     supportCase.CaseNumber,
		Title:          supportCase.Subject,
		Description:    supportCase.Description,
		Severity:       supportCase.Priority,
		Category:       supportCase.Category,
		Status:         supportCase.Status,
		AssignedUserID: supportCase.AssignedToID,
		Resolution:     supportCase.Resolution,
		CreatedAt:      supportCase.CreatedAt,
		UpdatedAt:      supportCase.UpdatedAt,
	}
}

func productCommercialConsentStateToResponse(state models.ConsentState) ConsentStateResponse {
	return ConsentStateResponse{
		ID:            state.ID,
		ContactID:     state.ContactID,
		SubjectType:   state.SubjectType,
		SubjectKey:    state.SubjectKey,
		Purpose:       state.Purpose,
		Channel:       state.Channel,
		Status:        state.Status,
		LatestEventID: state.LatestEventID,
		PolicyVersion: state.PolicyVersion,
		EffectiveAt:   state.EffectiveAt,
		ExpiresAt:     state.ExpiresAt,
	}
}

func productCommercialRetentionToResponse(policy models.RetentionPolicy) RetentionPolicyResponse {
	return RetentionPolicyResponse{
		ID:              policy.ID,
		DataCategory:    policy.ResourceType,
		RetentionDays:   policy.RetentionDays,
		GracePeriodDays: policy.GracePeriodDays,
		Action:          policy.Action,
		LegalBasis:      policy.LegalBasis,
		IsEnabled:       policy.IsActive,
		EffectiveFrom:   policy.EffectiveFrom,
	}
}

func productCommercialRecoveryToResponse(checkpoint models.RecoveryCheckpoint) RecoveryCheckpointResponse {
	return RecoveryCheckpointResponse{
		ID:         checkpoint.ID,
		Kind:       checkpoint.Type,
		Status:     checkpoint.Status,
		CreatedAt:  checkpoint.CreatedAt,
		ReadyAt:    checkpoint.ReadyAt,
		VerifiedAt: checkpoint.VerifiedAt,
		ExpiresAt:  checkpoint.ExpiresAt,
		SizeBytes:  checkpoint.SizeBytes,
	}
}

func productCommercialUserBelongsToOrganization(
	db *gorm.DB,
	organizationID, userID uuid.UUID,
) (bool, error) {
	if userID == uuid.Nil {
		return false, nil
	}
	var count int64
	err := db.Table("users").
		Joins("LEFT JOIN user_organizations ON user_organizations.user_id = users.id AND user_organizations.deleted_at IS NULL").
		Where(
			"users.id = ? AND users.deleted_at IS NULL AND users.is_active = ? AND (users.organization_id = ? OR user_organizations.organization_id = ?)",
			userID,
			true,
			organizationID,
			organizationID,
		).
		Count(&count).Error
	return count > 0, err
}

// ProductEntitlementKeyForResource maps protected product modules to their
// commercial feature key. Core inbox/WhatsApp resources remain outside this
// map so legacy behavior is unchanged.
func ProductEntitlementKeyForResource(resource string) (string, bool) {
	switch resource {
	case models.ResourceCRMPipelines,
		models.ResourceCRMLeads,
		models.ResourceCRMAutomations,
		models.ResourceTasks:
		return "crm.enabled", true
	case models.ResourceBookings, models.ResourceBookingSettings:
		return "bookings.enabled", true
	case models.ResourcePackages,
		models.ResourceCredits,
		models.ResourcePayments,
		models.ResourcePaymentRefunds:
		return "commerce.enabled", true
	case models.ResourceCopilot:
		return "copilot.enabled", true
	case models.ResourceChannelAccounts, models.ResourceConversations:
		return "omnichannel.enabled", true
	default:
		return "", false
	}
}

func productCommercialEntitlementAllows(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case int:
		return typed > 0
	case int32:
		return typed > 0
	case int64:
		return typed > 0
	case float64:
		return !math.IsNaN(typed) && typed > 0
	case string:
		normalized := strings.ToLower(strings.TrimSpace(typed))
		return normalized != "" &&
			normalized != "0" &&
			normalized != "false" &&
			normalized != "disabled" &&
			normalized != "none"
	case models.JSONB:
		if nested, ok := typed["value"]; ok {
			return productCommercialEntitlementAllows(nested)
		}
		if enabled, ok := typed["enabled"]; ok {
			return productCommercialEntitlementAllows(enabled)
		}
		return len(typed) > 0
	case map[string]any:
		return productCommercialEntitlementAllows(models.JSONB(typed))
	default:
		return value != nil
	}
}

func productCommercialSubscriptionPermitsFeatures(
	subscription *models.Subscription,
	now time.Time,
) bool {
	if subscription == nil {
		return false
	}
	switch subscription.Status {
	case models.SubscriptionStatusActive:
		return (subscription.CurrentPeriodEnd != nil && subscription.CurrentPeriodEnd.After(now)) ||
			(subscription.GraceUntil != nil && subscription.GraceUntil.After(now))
	case models.SubscriptionStatusTrialing:
		return subscription.TrialEndsAt != nil && subscription.TrialEndsAt.After(now)
	case models.SubscriptionStatusPastDue:
		return subscription.GraceUntil != nil && subscription.GraceUntil.After(now)
	default:
		return false
	}
}

var productCommercialLiveSubscriptionStatuses = []models.SubscriptionStatus{
	models.SubscriptionStatusIncomplete,
	models.SubscriptionStatusTrialing,
	models.SubscriptionStatusActive,
	models.SubscriptionStatusPastDue,
	models.SubscriptionStatusPaused,
}

// productCommercialLoadCurrentSubscription prefers the single live lifecycle
// row guaranteed by idx_subscriptions_org_live. If no live row exists, it falls
// back to the newest historical row so cancellation and expiry stay visible.
func productCommercialLoadCurrentSubscription(
	db *gorm.DB,
	organizationID uuid.UUID,
	subscription *models.Subscription,
) error {
	err := db.Where(
		"organization_id = ? AND status IN ?",
		organizationID,
		productCommercialLiveSubscriptionStatuses,
	).Order("created_at DESC, id DESC").First(subscription).Error
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	return db.Where("organization_id = ?", organizationID).
		Order("created_at DESC, id DESC").
		First(subscription).Error
}

func productCommercialActiveOverride(
	db *gorm.DB,
	orgID uuid.UUID,
	key string,
	now time.Time,
) (*models.EntitlementOverride, error) {
	var override models.EntitlementOverride
	err := db.Where(
		"organization_id = ? AND key = ? AND is_active = ? AND starts_at <= ? AND (expires_at IS NULL OR expires_at > ?)",
		orgID,
		key,
		true,
		now,
		now,
	).Order("starts_at DESC, created_at DESC").First(&override).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &override, nil
}

// EvaluateProductEntitlement evaluates one server-side feature decision.
//
// Semantics:
//   - commercial gating applies to every tenant actor, including platform
//     administrators, so asynchronous work cannot outlive a bypass;
//   - organizations with no subscription fail closed;
//   - active/trialing/past-due subscriptions use only their immutable
//     entitlements_snapshot, never the mutable plan catalog;
//   - incomplete, paused, canceled, and expired subscriptions fail closed;
//   - a currently active organization override replaces the snapshot value,
//     except that it cannot reopen a fail-closed subscription.
func (a *App) EvaluateProductEntitlement(
	_ uuid.UUID, orgID uuid.UUID,
	key string,
) (ProductEntitlementDecision, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	decision := ProductEntitlementDecision{
		Key:     key,
		Mode:    "subscription",
		Reason:  "Entitlement is not enabled",
		Allowed: false,
	}
	if orgID == uuid.Nil {
		return decision, errors.New("organization ID is required")
	}
	if !productCommercialValidIdentifier(key, 150) {
		return decision, errors.New("invalid entitlement key")
	}
	now := time.Now().UTC()
	var subscription models.Subscription
	err := productCommercialLoadCurrentSubscription(a.DB, orgID, &subscription)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		decision.Mode = "unlicensed"
		decision.Reason = "No commercial subscription is assigned"
		return decision, nil
	}
	if err != nil {
		return decision, err
	}
	decision.SubscriptionID = &subscription.ID
	decision.SubscriptionStatus = productCommercialEffectiveSubscriptionStatus(
		&subscription,
		now,
	)
	if !productCommercialSubscriptionPermitsFeatures(&subscription, now) {
		decision.Mode = "suspended"
		decision.Reason = "Subscription status does not permit product features"
		return decision, nil
	}

	value, exists := subscription.EntitlementsSnapshot[key]
	if exists {
		decision.Value = value
		decision.Allowed = productCommercialEntitlementAllows(value)
		if decision.Allowed {
			decision.Reason = "Enabled by subscription snapshot"
		}
	}
	override, err := productCommercialActiveOverride(a.DB, orgID, key, now)
	if err != nil {
		return decision, err
	}
	if override != nil {
		decision.Value = productCommercialEntitlementValue(override.Value)
		decision.Allowed = productCommercialEntitlementAllows(decision.Value)
		decision.Overridden = true
		decision.Reason = "Active organization override"
	}
	return decision, nil
}

// HasProductEntitlement is the boolean convenience wrapper for authorization middleware.
func (a *App) HasProductEntitlement(
	userID, orgID uuid.UUID,
	key string,
) (bool, error) {
	decision, err := a.EvaluateProductEntitlement(userID, orgID, key)
	return decision.Allowed, err
}

func productCommercialPlanPriceAssignable(price *models.PlanPrice) bool {
	if price == nil || price.Metadata == nil {
		return true
	}
	assignable, configured := price.Metadata["assignable"].(bool)
	return !configured || assignable
}

func (a *App) productCommercialPlanCatalog(
	organization *models.Organization,
	publicOnly bool,
	assignableOnly bool,
) ([]ProductPlanResponse, error) {
	query := a.DB.Where("status = ?", models.CommercialPlanStatusActive)
	if publicOnly {
		query = query.Where("is_public = ?", true)
	}
	if organization.ResellerID != nil {
		query = query.Where("reseller_id IS NULL OR reseller_id = ?", *organization.ResellerID)
	} else {
		query = query.Where("reseller_id IS NULL")
	}

	var plans []models.Plan
	if err := query.Order("display_order ASC, name ASC").Find(&plans).Error; err != nil {
		return nil, err
	}
	if len(plans) == 0 {
		return []ProductPlanResponse{}, nil
	}

	planIDs := make([]uuid.UUID, len(plans))
	for i := range plans {
		planIDs[i] = plans[i].ID
	}
	var entitlements []models.PlanEntitlement
	if err := a.DB.Where("plan_id IN ?", planIDs).
		Order("key ASC").
		Find(&entitlements).Error; err != nil {
		return nil, err
	}
	var prices []models.PlanPrice
	now := time.Now().UTC()
	priceQuery := a.DB.Where(
		"plan_id IN ? AND is_active = ? AND (effective_from IS NULL OR effective_from <= ?) AND (effective_until IS NULL OR effective_until > ?)",
		planIDs,
		true,
		now,
		now,
	)
	if assignableOnly {
		priceQuery = priceQuery.Where(
			"provider = ? AND interval IN ?",
			models.BillingProviderManual,
			[]models.BillingInterval{
				models.BillingIntervalMonth,
				models.BillingIntervalYear,
			},
		)
	}
	if err := priceQuery.Order("unit_amount_minor ASC, code ASC").Find(&prices).Error; err != nil {
		return nil, err
	}

	entitlementsByPlan := make(map[uuid.UUID]map[string]any)
	for _, entitlement := range entitlements {
		if entitlementsByPlan[entitlement.PlanID] == nil {
			entitlementsByPlan[entitlement.PlanID] = make(map[string]any)
		}
		entitlementsByPlan[entitlement.PlanID][entitlement.Key] =
			productCommercialEntitlementValue(entitlement.Value)
	}
	pricesByPlan := make(map[uuid.UUID][]ProductPlanPriceResponse)
	for _, price := range prices {
		if assignableOnly && !productCommercialPlanPriceAssignable(&price) {
			continue
		}
		priceID := price.ID
		pricesByPlan[price.PlanID] = append(pricesByPlan[price.PlanID], ProductPlanPriceResponse{
			ID:               &priceID,
			Code:             price.Code,
			Currency:         price.Currency,
			UnitAmountMinor:  price.UnitAmountMinor,
			SetupAmountMinor: price.SetupAmountMinor,
			Interval:         price.Interval,
			IntervalCount:    price.IntervalCount,
			TaxBehavior:      price.TaxBehavior,
			Assignable:       productCommercialPlanPriceAssignable(&price),
		})
	}

	response := make([]ProductPlanResponse, 0, len(plans))
	for i := range plans {
		entitlementMap := entitlementsByPlan[plans[i].ID]
		if entitlementMap == nil {
			entitlementMap = map[string]any{}
		}
		planPrices := pricesByPlan[plans[i].ID]
		if planPrices == nil {
			planPrices = []ProductPlanPriceResponse{}
		}
		if assignableOnly && len(planPrices) == 0 {
			continue
		}
		planID := plans[i].ID
		response = append(response, ProductPlanResponse{
			ID:           &planID,
			Code:         plans[i].Code,
			Name:         plans[i].Name,
			Description:  plans[i].Description,
			Vertical:     plans[i].Vertical,
			Status:       plans[i].Status,
			TrialDays:    plans[i].TrialDays,
			IsPublic:     plans[i].IsPublic,
			Entitlements: entitlementMap,
			Prices:       planPrices,
		})
	}
	return response, nil
}

// ListProductPlans returns public plans visible to the current organization's reseller.
func (a *App) ListProductPlans(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceBilling, models.ActionRead)
	if err != nil {
		return nil
	}

	var organization models.Organization
	if err := a.DB.Select("id", "reseller_id").
		Where("id = ?", orgID).
		First(&organization).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Organization not found", nil, "")
	}
	response, err := a.productCommercialPlanCatalog(&organization, true, false)
	if err != nil {
		return a.sendProductCommercialError(r, "list product plans", err)
	}
	return r.SendEnvelope(map[string]any{"plans": response})
}

// ListAssignableProductPlans returns the exact manual recurring catalog that a
// platform owner may assign to one target organization. Private plans remain
// hidden from reseller administrators and from the tenant-facing catalog.
func (a *App) ListAssignableProductPlans(r *fastglue.Request) error {
	_, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.IsSuperAdmin(userID) {
		return r.SendErrorEnvelope(
			fasthttp.StatusForbidden,
			"Platform owner access required",
			nil,
			"",
		)
	}
	targetOrgID, err := targetOrganizationID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid organization ID", nil, "")
	}
	var organization models.Organization
	if err := a.DB.Select("id", "reseller_id").
		Where("id = ?", targetOrgID).
		First(&organization).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Organization not found", nil, "")
		}
		return a.sendProductCommercialError(r, "load organization plan catalog", err)
	}
	response, err := a.productCommercialPlanCatalog(&organization, false, true)
	if err != nil {
		return a.sendProductCommercialError(r, "list assignable product plans", err)
	}
	return r.SendEnvelope(map[string]any{"plans": response})
}

// GetProductSubscription returns the current tenant subscription without provider secrets.
func (a *App) GetProductSubscription(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceBilling, models.ActionRead)
	if err != nil {
		return nil
	}

	var subscription models.Subscription
	err = productCommercialLoadCurrentSubscription(a.DB, orgID, &subscription)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return r.SendEnvelope(productCommercialSubscriptionToResponse(nil, nil))
	}
	if err != nil {
		return a.sendProductCommercialError(r, "load product subscription", err)
	}

	var plan models.Plan
	if err := a.DB.Where("id = ?", subscription.PlanID).First(&plan).Error; err != nil &&
		!errors.Is(err, gorm.ErrRecordNotFound) {
		return a.sendProductCommercialError(r, "load subscription plan", err)
	}
	return r.SendEnvelope(productCommercialSubscriptionToResponse(&subscription, &plan))
}

// GetProductEntitlements computes plan entitlements plus active tenant overrides.
func (a *App) GetProductEntitlements(r *fastglue.Request) error {
	// Every authenticated tenant member needs the effective, non-sensitive
	// feature projection so clients can hide or lock modules their
	// organization has not licensed. Managing overrides remains protected by
	// the entitlements write permission on the platform control-plane routes.
	orgID, _, err := a.getOrgAndUserID(r)
	if err != nil {
		_ = r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
		return nil
	}

	now := time.Now().UTC()
	response := ProductEntitlementsResponse{
		PlanCode:       "unlicensed",
		Mode:           "unlicensed",
		Entitlements:   map[string]any{},
		OverriddenKeys: []string{},
		EvaluatedAt:    now,
	}

	var subscription models.Subscription
	err = productCommercialLoadCurrentSubscription(a.DB, orgID, &subscription)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return a.sendProductCommercialError(r, "load product entitlements", err)
	}
	applyOverrides := false
	if err == nil {
		response.Mode = "subscription"
		response.SubscriptionStatus = productCommercialEffectiveSubscriptionStatus(
			&subscription,
			now,
		)
		var plan models.Plan
		if err := a.DB.Where("id = ?", subscription.PlanID).First(&plan).Error; err == nil {
			response.PlanCode = plan.Code
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return a.sendProductCommercialError(r, "load entitlement plan", err)
		}
		if productCommercialSubscriptionPermitsFeatures(&subscription, now) {
			response.Entitlements = productCommercialJSONCopy(subscription.EntitlementsSnapshot)
			applyOverrides = true
		} else {
			response.Mode = "suspended"
			response.Entitlements = map[string]any{}
		}
	}

	if applyOverrides {
		var overrides []models.EntitlementOverride
		if err := a.DB.Where(
			"organization_id = ? AND is_active = ? AND starts_at <= ? AND (expires_at IS NULL OR expires_at > ?)",
			orgID,
			true,
			now,
			now,
		).Order("starts_at ASC, created_at ASC").Find(&overrides).Error; err != nil {
			return a.sendProductCommercialError(r, "load entitlement overrides", err)
		}
		for _, override := range overrides {
			response.Entitlements[override.Key] = productCommercialEntitlementValue(override.Value)
			response.OverriddenKeys = append(response.OverriddenKeys, override.Key)
		}
	}
	sort.Strings(response.OverriddenKeys)
	return r.SendEnvelope(response)
}

func productCommercialValidateProfile(profile models.JSONB) error {
	if profile == nil {
		return errors.New("profile is required")
	}
	data, err := json.Marshal(profile)
	if err != nil {
		return errors.New("profile must contain valid JSON values")
	}
	if len(data) > productCommercialMaxProfileBytes {
		return fmt.Errorf("profile must be at most %d bytes", productCommercialMaxProfileBytes)
	}
	if len(profile) > 64 {
		return errors.New("profile may contain at most 64 top-level fields")
	}
	if err := productCommercialRejectSensitiveJSON(profile, "profile"); err != nil {
		return err
	}
	if vertical := strings.ToLower(productCommercialString(profile["vertical"])); vertical != "" {
		switch vertical {
		case "clinic", "pharmacy", "wellness", "general":
		default:
			return errors.New("vertical must be clinic, pharmacy, wellness, or general")
		}
		profile["vertical"] = vertical
	}
	if rawChannels, declared := profile["intended_channels"]; declared {
		channels, err := productCommercialNormalizeIntendedChannels(rawChannels)
		if err != nil {
			return err
		}
		if len(channels) == 0 {
			return errors.New("intended_channels must contain at least one supported channel")
		}
		normalized := make([]string, len(channels))
		for index := range channels {
			normalized[index] = string(channels[index])
		}
		profile["intended_channels"] = normalized
	}
	return nil
}

func productCommercialNormalizeIntendedChannels(value any) ([]models.Channel, error) {
	var values []string
	switch typed := value.(type) {
	case []string:
		values = append(values, typed...)
	case []any:
		values = make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, errors.New("intended_channels must contain only channel names")
			}
			values = append(values, text)
		}
	case nil:
		return []models.Channel{}, nil
	default:
		return nil, errors.New("intended_channels must be an array of channel names")
	}

	requested := make(map[models.Channel]struct{}, len(values))
	for _, value := range values {
		channel := models.Channel(strings.ToLower(strings.TrimSpace(value)))
		supported := false
		for _, candidate := range productCommercialIntendedChannelOrder {
			if channel == candidate {
				supported = true
				break
			}
		}
		if !supported {
			return nil, fmt.Errorf("unsupported intended channel %q", value)
		}
		requested[channel] = struct{}{}
	}

	channels := make([]models.Channel, 0, len(requested))
	for _, channel := range productCommercialIntendedChannelOrder {
		if _, exists := requested[channel]; exists {
			channels = append(channels, channel)
		}
	}
	return channels, nil
}

func productCommercialIntendedChannels(
	profile models.JSONB,
) ([]models.Channel, bool, error) {
	if profile == nil {
		return nil, false, nil
	}
	value, declared := profile["intended_channels"]
	if !declared {
		return nil, false, nil
	}
	channels, err := productCommercialNormalizeIntendedChannels(value)
	return channels, true, err
}

func productCommercialProfileComplete(profile models.JSONB) bool {
	if profile == nil {
		return false
	}
	businessName := strings.TrimSpace(productCommercialString(profile["business_name"]))
	if businessName == "" {
		switch business := profile["business"].(type) {
		case map[string]any:
			businessName = strings.TrimSpace(productCommercialString(business["name"]))
		case models.JSONB:
			businessName = strings.TrimSpace(productCommercialString(business["name"]))
		}
	}
	timezone := strings.TrimSpace(productCommercialString(profile["timezone"]))
	if businessName == "" || timezone == "" {
		return false
	}
	if channels, declared, err := productCommercialIntendedChannels(profile); err != nil || (declared && len(channels) == 0) {
		return false
	}
	_, err := time.LoadLocation(timezone)
	return err == nil
}

func productCommercialRejectSensitiveJSON(value any, path string) error {
	switch typed := value.(type) {
	case models.JSONB:
		for key, nested := range typed {
			normalized := strings.ToLower(strings.TrimSpace(key))
			for _, forbidden := range []string{
				"password",
				"secret",
				"api_key",
				"access_token",
				"refresh_token",
				"private_key",
				"credential",
			} {
				if strings.Contains(normalized, forbidden) {
					return fmt.Errorf("%s.%s must not contain credentials or secrets", path, key)
				}
			}
			if err := productCommercialRejectSensitiveJSON(nested, path+"."+key); err != nil {
				return err
			}
		}
	case map[string]any:
		return productCommercialRejectSensitiveJSON(models.JSONB(typed), path)
	case []any:
		for i, nested := range typed {
			if err := productCommercialRejectSensitiveJSON(nested, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func productCommercialManualOnboardingStep(key string) bool {
	return key == "workspace_profile" || key == "go_live"
}

func productCommercialOnboardingAuditSnapshot(onboarding *models.OrganizationOnboarding) map[string]any {
	vertical := "general"
	if onboarding != nil {
		if value := productCommercialString(onboarding.Input["vertical"]); value != "" {
			vertical = value
		}
		return map[string]any{
			"status":           onboarding.Status,
			"current_step":     onboarding.CurrentStep,
			"progress_percent": onboarding.ProgressPercent,
			"vertical":         vertical,
			"template_id":      onboarding.TemplateID,
		}
	}
	return map[string]any{}
}

func productCommercialLoadOrCreateOnboarding(
	tx *gorm.DB,
	orgID, userID uuid.UUID,
) (*models.OrganizationOnboarding, error) {
	var onboarding models.OrganizationOnboarding
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("organization_id = ?", orgID).
		First(&onboarding).Error
	if err == nil {
		if onboarding.Checklist == nil {
			onboarding.Checklist = models.JSONB{}
		}
		if onboarding.Input == nil {
			onboarding.Input = models.JSONB{}
		}
		return &onboarding, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	now := time.Now().UTC()
	onboarding = models.OrganizationOnboarding{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgID,
		Status:          models.OnboardingStatusInProgress,
		CurrentStep:     productOnboardingStepDefinitions[0].key,
		ProgressPercent: 0,
		Checklist:       models.JSONB{},
		Input:           models.JSONB{},
		Metadata:        models.JSONB{},
		RequestedByID:   &userID,
		OwnerUserID:     &userID,
		StartedAt:       &now,
	}
	if err := tx.Create(&onboarding).Error; err != nil {
		return nil, err
	}
	return &onboarding, nil
}

func productCommercialReadyChannels(
	db *gorm.DB,
	orgID uuid.UUID,
) (map[models.Channel]bool, error) {
	ready := make(map[models.Channel]bool, len(productCommercialIntendedChannelOrder))
	if db.Migrator().HasTable("whatsapp_accounts") {
		var whatsAppCount int64
		if err := db.Table("whatsapp_accounts").
			Where("organization_id = ? AND status = ? AND deleted_at IS NULL", orgID, "active").
			Count(&whatsAppCount).Error; err != nil {
			return nil, err
		}
		ready[models.ChannelWhatsApp] = whatsAppCount > 0
	}
	if db.Migrator().HasTable("channel_accounts") {
		var rows []struct {
			Channel models.Channel `gorm:"column:channel"`
		}
		if err := db.Table("channel_accounts").
			Select("DISTINCT channel").
			Where(
				"organization_id = ? AND status = ? AND deleted_at IS NULL AND provider <> ? AND LOWER(COALESCE(config->>'outbound_enabled', 'false')) = 'true' AND last_health_check_at IS NOT NULL AND COALESCE(last_error, '') = ''",
				orgID,
				models.ChannelAccountStatusActive,
				"meta_legacy",
			).
			Scan(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			ready[row.Channel] = true
		}
	}
	return ready, nil
}

func productCommercialWorkspaceIntendedChannels(
	db *gorm.DB,
	orgID uuid.UUID,
) ([]models.Channel, bool, error) {
	var onboarding models.OrganizationOnboarding
	err := db.Select("input").
		Where("organization_id = ?", orgID).
		First(&onboarding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return productCommercialIntendedChannels(onboarding.Input)
}

func productCommercialChannelLabel(channel models.Channel) string {
	switch channel {
	case models.ChannelWhatsApp:
		return "WhatsApp"
	case models.ChannelInstagram:
		return "Instagram"
	case models.ChannelMessenger:
		return "Messenger"
	case models.ChannelThreads:
		return "Threads"
	case models.ChannelEmail:
		return "email"
	case models.ChannelWebChat:
		return "web chat"
	default:
		return string(channel)
	}
}

func productCommercialOnboardingSignals(
	db *gorm.DB,
	orgID uuid.UUID,
	onboarding *models.OrganizationOnboarding,
) (map[string]bool, error) {
	signals := map[string]bool{}
	var onboardingProfile models.JSONB
	if onboarding != nil {
		onboardingProfile = onboarding.Input
		signals["workspace_profile"] =
			productCommercialJSONBool(onboarding.Checklist, "workspace_profile") &&
				productCommercialProfileComplete(onboarding.Input)
		signals["go_live"] = productCommercialJSONBool(onboarding.Checklist, "go_live")
	}
	intendedChannels, declared, err := productCommercialIntendedChannels(onboardingProfile)
	if err != nil {
		return nil, err
	}

	var subscription models.Subscription
	if err := productCommercialLoadCurrentSubscription(db, orgID, &subscription); err == nil {
		licenseReady, licenseErr := productCommercialLicenseReadyForChannels(
			db,
			orgID,
			&subscription,
			intendedChannels,
			declared,
			time.Now().UTC(),
		)
		if licenseErr != nil {
			return nil, licenseErr
		}
		signals["license_assigned"] = licenseReady
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	var templateCount int64
	if err := db.Model(&models.WorkspaceTemplateApplication{}).
		Where(
			"organization_id = ? AND status = ?",
			orgID,
			models.TemplateApplicationStatusSucceeded,
		).
		Count(&templateCount).Error; err != nil {
		return nil, err
	}
	signals["vertical_template"] = templateCount > 0

	readyChannels, err := productCommercialReadyChannels(db, orgID)
	if err != nil {
		return nil, err
	}
	if declared {
		allReady := len(intendedChannels) > 0
		for _, channel := range intendedChannels {
			if !readyChannels[channel] {
				allReady = false
				break
			}
		}
		signals["channel_connected"] = allReady
	} else {
		for _, connected := range readyChannels {
			if connected {
				signals["channel_connected"] = true
				break
			}
		}
	}

	var userCount int64
	if err := db.Table("users").
		Joins("LEFT JOIN user_organizations ON user_organizations.user_id = users.id AND user_organizations.deleted_at IS NULL").
		Where(
			"users.deleted_at IS NULL AND users.is_active = ? AND (users.organization_id = ? OR user_organizations.organization_id = ?)",
			true,
			orgID,
			orgID,
		).
		Distinct("users.id").
		Count(&userCount).Error; err != nil {
		return nil, err
	}
	signals["team_invited"] = userCount > 1

	var retentionCount int64
	if err := db.Model(&models.RetentionPolicy{}).
		Where("organization_id = ? AND is_active = ?", orgID, true).
		Count(&retentionCount).Error; err != nil {
		return nil, err
	}
	signals["privacy_baseline"] = retentionCount > 0
	return signals, nil
}

func productCommercialLicenseReadyForChannels(
	db *gorm.DB,
	orgID uuid.UUID,
	subscription *models.Subscription,
	intendedChannels []models.Channel,
	declared bool,
	now time.Time,
) (bool, error) {
	if !productCommercialSubscriptionPermitsFeatures(subscription, now) {
		return false, nil
	}
	// Preserve the historical onboarding behavior for existing workspaces that
	// predate intended-channel declarations. New workspaces declare the list
	// explicitly and therefore fail closed until at least one channel is chosen.
	if !declared {
		return true, nil
	}
	if len(intendedChannels) == 0 {
		return false, nil
	}

	omnichannelAllowed, err := productCommercialSubscriptionAllowsEntitlement(
		db,
		orgID,
		subscription,
		channelapi.OmnichannelEntitlementKey,
		now,
	)
	if err != nil || !omnichannelAllowed {
		return false, err
	}
	for _, channel := range intendedChannels {
		if channel != models.ChannelThreads {
			continue
		}
		return productCommercialSubscriptionAllowsEntitlement(
			db,
			orgID,
			subscription,
			channelapi.ThreadsPublicEngagementEntitlementKey,
			now,
		)
	}
	return true, nil
}

func productCommercialSubscriptionAllowsEntitlement(
	db *gorm.DB,
	orgID uuid.UUID,
	subscription *models.Subscription,
	key string,
	now time.Time,
) (bool, error) {
	if !productCommercialSubscriptionPermitsFeatures(subscription, now) {
		return false, nil
	}
	allowed := productCommercialEntitlementAllows(subscription.EntitlementsSnapshot[key])
	override, err := productCommercialActiveOverride(db, orgID, key, now)
	if err != nil {
		return false, err
	}
	if override != nil {
		allowed = productCommercialEntitlementAllows(override.Value)
	}
	return allowed, nil
}

func productCommercialBuildOnboardingResponse(
	db *gorm.DB,
	orgID uuid.UUID,
	onboarding *models.OrganizationOnboarding,
) (OnboardingResponse, error) {
	signals, err := productCommercialOnboardingSignals(db, orgID, onboarding)
	if err != nil {
		return OnboardingResponse{}, err
	}

	response := OnboardingResponse{
		Vertical: "general",
		Status:   models.OnboardingStatusNotStarted,
		Profile:  models.JSONB{},
		Steps:    make([]OnboardingStepResponse, 0, len(productOnboardingStepDefinitions)),
	}
	if onboarding != nil {
		response.ID = &onboarding.ID
		response.Status = onboarding.Status
		response.Profile = productCommercialJSONCopy(onboarding.Input)
		if vertical := productCommercialString(onboarding.Input["vertical"]); vertical != "" {
			response.Vertical = vertical
		}
	}

	completedCount := 0
	for _, definition := range productOnboardingStepDefinitions {
		completed := signals[definition.key]
		if completed {
			completedCount++
		} else if response.CurrentStep == "" {
			response.CurrentStep = definition.key
		}
		response.Steps = append(response.Steps, OnboardingStepResponse{
			Key:         definition.key,
			Label:       definition.label,
			Description: definition.description,
			Completed:   completed,
			Inferred:    definition.inferred,
			ActionPath:  definition.actionPath,
		})
	}
	response.ProgressPercent = completedCount * 100 / len(productOnboardingStepDefinitions)
	if completedCount == len(productOnboardingStepDefinitions) {
		response.Status = models.OnboardingStatusReady
		response.CurrentStep = ""
	} else if completedCount > 0 || onboarding != nil {
		if response.Status != models.OnboardingStatusFailed &&
			response.Status != models.OnboardingStatusCanceled {
			response.Status = models.OnboardingStatusInProgress
		}
	}
	response.ProvisioningState = "onboarding_required"
	completedByKey := make(map[string]bool, len(response.Steps))
	for _, step := range response.Steps {
		completedByKey[step.Key] = step.Completed
	}
	switch {
	case response.Status == models.OnboardingStatusReady:
		response.ProvisioningState = "ready"
	case !completedByKey["workspace_profile"]:
		response.ProvisioningState = "profile_required"
	case !completedByKey["license_assigned"]:
		response.ProvisioningState = "license_required"
	case !completedByKey["channel_connected"]:
		response.ProvisioningState = "authorization_required"
	}
	return response, nil
}

func productCommercialPersistOnboardingProgress(
	tx *gorm.DB,
	onboarding *models.OrganizationOnboarding,
	response OnboardingResponse,
) error {
	onboarding.Status = response.Status
	onboarding.CurrentStep = response.CurrentStep
	onboarding.ProgressPercent = response.ProgressPercent
	onboarding.Metadata = productCommercialJSONCopy(onboarding.Metadata)
	onboarding.Metadata["provisioning_state"] = response.ProvisioningState
	updates := map[string]any{
		"status":              onboarding.Status,
		"current_step":        onboarding.CurrentStep,
		"progress_percent":    onboarding.ProgressPercent,
		"checklist":           onboarding.Checklist,
		"input":               onboarding.Input,
		"metadata":            onboarding.Metadata,
		"template_id":         onboarding.TemplateID,
		"template_version_id": onboarding.TemplateVersionID,
	}
	if response.Status == models.OnboardingStatusReady && onboarding.CompletedAt == nil {
		now := time.Now().UTC()
		onboarding.CompletedAt = &now
		updates["completed_at"] = now
	}
	return tx.Model(&models.OrganizationOnboarding{}).
		Where("id = ? AND organization_id = ?", onboarding.ID, onboarding.OrganizationID).
		Updates(updates).Error
}

// GetOnboarding returns persisted and inferred tenant go-live state.
func (a *App) GetOnboarding(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceOnboarding, models.ActionRead)
	if err != nil {
		return nil
	}

	var onboarding models.OrganizationOnboarding
	err = a.DB.Where("organization_id = ?", orgID).First(&onboarding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		response, buildErr := productCommercialBuildOnboardingResponse(a.DB, orgID, nil)
		if buildErr != nil {
			return a.sendProductCommercialError(r, "load onboarding", buildErr)
		}
		return r.SendEnvelope(response)
	}
	if err != nil {
		return a.sendProductCommercialError(r, "load onboarding", err)
	}
	response, err := productCommercialBuildOnboardingResponse(a.DB, orgID, &onboarding)
	if err != nil {
		return a.sendProductCommercialError(r, "load onboarding milestones", err)
	}
	return r.SendEnvelope(response)
}

// UpdateOnboardingProfile stores a credential-free provisioning profile.
func (a *App) UpdateOnboardingProfile(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceOnboarding, models.ActionWrite)
	if err != nil {
		return nil
	}
	var req UpdateOnboardingProfileRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := productCommercialValidateProfile(req.Profile); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	var response OnboardingResponse
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		onboarding, err := productCommercialLoadOrCreateOnboarding(tx, orgID, userID)
		if err != nil {
			return err
		}
		oldSnapshot := productCommercialOnboardingAuditSnapshot(onboarding)
		onboarding.Input = productCommercialJSONCopy(req.Profile)
		onboarding.Checklist = productCommercialJSONCopy(onboarding.Checklist)
		onboarding.Checklist["workspace_profile"] = productCommercialProfileComplete(onboarding.Input)

		response, err = productCommercialBuildOnboardingResponse(tx, orgID, onboarding)
		if err != nil {
			return err
		}
		if err := productCommercialPersistOnboardingProgress(tx, onboarding, response); err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			productOnboardingAuditResource,
			onboarding.ID,
			models.AuditActionUpdated,
			oldSnapshot,
			productCommercialOnboardingAuditSnapshot(onboarding),
		)
	})
	if err != nil {
		return a.sendProductCommercialError(r, "update onboarding profile", err)
	}
	return r.SendEnvelope(response)
}

// CompleteOnboardingStep marks a manual milestone. Inferred milestones cannot
// be overridden, and go-live is blocked until every prerequisite is complete.
func (a *App) CompleteOnboardingStep(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceOnboarding, models.ActionWrite)
	if err != nil {
		return nil
	}
	key, _ := r.RequestCtx.UserValue("key").(string)
	key = strings.ToLower(strings.TrimSpace(key))
	if !productCommercialManualOnboardingStep(key) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid or automatically inferred onboarding step", nil, "")
	}

	var response OnboardingResponse
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		onboarding, err := productCommercialLoadOrCreateOnboarding(tx, orgID, userID)
		if err != nil {
			return err
		}
		oldSnapshot := productCommercialOnboardingAuditSnapshot(onboarding)
		if key == "workspace_profile" && !productCommercialProfileComplete(onboarding.Input) {
			return newProductCommercialClientError(
				fasthttp.StatusConflict,
				"Add the business name and a valid IANA timezone before completing this milestone",
			)
		}
		if key == "go_live" {
			signals, err := productCommercialOnboardingSignals(tx, orgID, onboarding)
			if err != nil {
				return err
			}
			for _, definition := range productOnboardingStepDefinitions {
				if definition.key == "go_live" {
					continue
				}
				if !signals[definition.key] {
					return newProductCommercialClientError(
						fasthttp.StatusConflict,
						"Complete every go-live prerequisite before approval",
					)
				}
			}
		}

		onboarding.Checklist = productCommercialJSONCopy(onboarding.Checklist)
		onboarding.Checklist[key] = true
		response, err = productCommercialBuildOnboardingResponse(tx, orgID, onboarding)
		if err != nil {
			return err
		}
		if err := productCommercialPersistOnboardingProgress(tx, onboarding, response); err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			productOnboardingAuditResource,
			onboarding.ID,
			models.AuditActionUpdated,
			oldSnapshot,
			productCommercialOnboardingAuditSnapshot(onboarding),
		)
	})
	if err != nil {
		return a.sendProductCommercialError(r, "complete onboarding step", err)
	}
	return r.SendEnvelope(response)
}

// ListWorkspaceTemplates returns published reseller/global templates and
// built-in vertical templates when no persisted version exists.
func (a *App) ListWorkspaceTemplates(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceWorkspaceTemplates, models.ActionRead)
	if err != nil {
		return nil
	}

	var organization models.Organization
	if err := a.DB.Select("id", "reseller_id").
		Where("id = ?", orgID).
		First(&organization).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Organization not found", nil, "")
	}

	query := a.DB.Where("status = ?", models.WorkspaceTemplateStatusPublished)
	if organization.ResellerID != nil {
		query = query.Where("reseller_id IS NULL OR reseller_id = ?", *organization.ResellerID)
	} else {
		query = query.Where("reseller_id IS NULL")
	}
	var templates []models.WorkspaceTemplate
	if err := query.Order("name ASC").Find(&templates).Error; err != nil {
		return a.sendProductCommercialError(r, "list workspace templates", err)
	}

	responseByKey := make(map[string]WorkspaceTemplateResponse)
	responseIsResellerOwned := make(map[string]bool)
	for _, template := range templates {
		var version models.WorkspaceTemplateVersion
		err := a.DB.Where(
			"template_id = ? AND status = ?",
			template.ID,
			models.WorkspaceTemplateStatusPublished,
		).Order("version DESC").First(&version).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return a.sendProductCommercialError(r, "load workspace template version", err)
		}
		isResellerOwned := template.ResellerID != nil
		if existingIsResellerOwned, exists := responseIsResellerOwned[template.Slug]; exists && existingIsResellerOwned && !isResellerOwned {
			continue
		}
		responseByKey[template.Slug] = productCommercialTemplateToResponse(template, version)
		responseIsResellerOwned[template.Slug] = isResellerOwned
	}
	for _, template := range productBuiltInWorkspaceTemplates {
		if _, exists := responseByKey[template.Key]; !exists {
			responseByKey[template.Key] = productCommercialBuiltinToResponse(template)
		}
	}

	response := make([]WorkspaceTemplateResponse, 0, len(responseByKey))
	for _, builtIn := range productBuiltInWorkspaceTemplates {
		response = append(response, responseByKey[builtIn.Key])
		delete(responseByKey, builtIn.Key)
	}
	custom := make([]WorkspaceTemplateResponse, 0, len(responseByKey))
	for _, template := range responseByKey {
		custom = append(custom, template)
	}
	sort.Slice(custom, func(i, j int) bool {
		return strings.ToLower(custom[i].Name) < strings.ToLower(custom[j].Name)
	})
	response = append(response, custom...)
	return r.SendEnvelope(map[string]any{"templates": response})
}

func productCommercialFindOrSeedTemplate(
	tx *gorm.DB,
	organization *models.Organization,
	key string,
	userID uuid.UUID,
) (*models.WorkspaceTemplate, *models.WorkspaceTemplateVersion, error) {
	query := tx.Where("slug = ? AND status = ?", key, models.WorkspaceTemplateStatusPublished)
	if organization.ResellerID != nil {
		query = query.Where("reseller_id IS NULL OR reseller_id = ?", *organization.ResellerID).
			Order(clause.Expr{
				SQL:  "CASE WHEN reseller_id = ? THEN 0 ELSE 1 END",
				Vars: []any{*organization.ResellerID},
			})
	} else {
		query = query.Where("reseller_id IS NULL")
	}

	var template models.WorkspaceTemplate
	err := query.First(&template).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		builtIn, ok := productCommercialBuiltInTemplate(key)
		if !ok {
			return nil, nil, newProductCommercialClientError(
				fasthttp.StatusNotFound,
				"Workspace template not found",
			)
		}

		templateAttrs := models.WorkspaceTemplate{
			BaseModel:   models.BaseModel{ID: uuid.New()},
			ScopeKey:    "platform",
			Slug:        builtIn.Key,
			Name:        builtIn.Name,
			Description: builtIn.Description,
			Vertical:    builtIn.Vertical,
			Status:      models.WorkspaceTemplateStatusPublished,
			IsDefault:   true,
			Tags:        models.StringArray{"built-in", builtIn.Vertical},
			Settings:    models.JSONB{},
			CreatedByID: &userID,
		}
		template = models.WorkspaceTemplate{}
		if err := tx.Where("scope_key = ? AND slug = ?", "platform", builtIn.Key).
			Attrs(templateAttrs).
			FirstOrCreate(&template).Error; err != nil {
			return nil, nil, err
		}
		if template.Status != models.WorkspaceTemplateStatusPublished {
			return nil, nil, newProductCommercialClientError(
				fasthttp.StatusConflict,
				"The built-in template catalog entry is not published",
			)
		}
	} else if err != nil {
		return nil, nil, err
	}

	var version models.WorkspaceTemplateVersion
	err = tx.Where(
		"template_id = ? AND status = ?",
		template.ID,
		models.WorkspaceTemplateStatusPublished,
	).Order("version DESC").First(&version).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		builtIn, ok := productCommercialBuiltInTemplate(key)
		if !ok || template.ScopeKey != "platform" {
			return nil, nil, newProductCommercialClientError(
				fasthttp.StatusConflict,
				"Workspace template has no published version",
			)
		}
		now := time.Now().UTC()
		versionAttrs := models.WorkspaceTemplateVersion{
			BaseModel:     models.BaseModel{ID: uuid.New()},
			TemplateID:    template.ID,
			Version:       1,
			Status:        models.WorkspaceTemplateStatusPublished,
			SchemaVersion: "1",
			Manifest:      productCommercialJSONCopy(builtIn.Manifest),
			ChangeSummary: "Initial built-in vertical playbook",
			Checksum:      "builtin:" + builtIn.Key + ":v1",
			CreatedByID:   &userID,
			PublishedByID: &userID,
			PublishedAt:   &now,
		}
		version = models.WorkspaceTemplateVersion{}
		if err := tx.Where("template_id = ? AND version = ?", template.ID, 1).
			Attrs(versionAttrs).
			FirstOrCreate(&version).Error; err != nil {
			return nil, nil, err
		}
		if version.Status != models.WorkspaceTemplateStatusPublished {
			return nil, nil, newProductCommercialClientError(
				fasthttp.StatusConflict,
				"The built-in template version is not published",
			)
		}
	} else if err != nil {
		return nil, nil, err
	}
	if err := productCommercialRejectSensitiveJSON(version.Manifest, "template_manifest"); err != nil {
		return nil, nil, newProductCommercialClientError(
			fasthttp.StatusConflict,
			"Workspace template manifest contains disallowed credential fields",
		)
	}
	manifestBytes, err := json.Marshal(version.Manifest)
	if err != nil || len(manifestBytes) > 256*1024 {
		return nil, nil, newProductCommercialClientError(
			fasthttp.StatusConflict,
			"Workspace template manifest is invalid or too large",
		)
	}
	return &template, &version, nil
}

// ApplyWorkspaceTemplate installs one immutable template version. Locking the
// organization row serializes retries; the application lookup then makes the
// operation idempotent without relying on an unsafe check-then-create race.
func (a *App) ApplyWorkspaceTemplate(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceWorkspaceTemplates, models.ActionWrite)
	if err != nil {
		return nil
	}
	key, _ := r.RequestCtx.UserValue("key").(string)
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" || len(key) > 100 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid workspace template key", nil, "")
	}

	var response ApplyWorkspaceTemplateResponse
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var organization models.Organization
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", orgID).
			First(&organization).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return newProductCommercialClientError(fasthttp.StatusNotFound, "Organization not found")
			}
			return err
		}

		template, version, err := productCommercialFindOrSeedTemplate(tx, &organization, key, userID)
		if err != nil {
			return err
		}

		var existing models.WorkspaceTemplateApplication
		err = tx.Where(
			"organization_id = ? AND template_version_id = ? AND status IN ?",
			orgID,
			version.ID,
			[]models.TemplateApplicationStatus{
				models.TemplateApplicationStatusPending,
				models.TemplateApplicationStatusApplying,
				models.TemplateApplicationStatusSucceeded,
			},
		).Order("created_at DESC").First(&existing).Error
		if err == nil {
			response = ApplyWorkspaceTemplateResponse{
				ApplicationID: existing.ID,
				TemplateKey:   template.Slug,
				Version:       version.Version,
				Status:        string(existing.Status),
				Idempotent:    true,
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		onboarding, err := productCommercialLoadOrCreateOnboarding(tx, orgID, userID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		application := models.WorkspaceTemplateApplication{
			BaseModel:         models.BaseModel{ID: uuid.New()},
			OrganizationID:    orgID,
			TemplateID:        template.ID,
			TemplateVersionID: version.ID,
			OnboardingID:      &onboarding.ID,
			Mode:              "install",
			Status:            models.TemplateApplicationStatusSucceeded,
			ManifestSnapshot:  productCommercialJSONCopy(version.Manifest),
			RequestedByID:     &userID,
			RequestedAt:       now,
			CompletedAt:       &now,
		}
		if err := tx.Create(&application).Error; err != nil {
			return err
		}

		provisioned := productTemplateProvisioningSummary{}
		if builtIn, ok := productCommercialBuiltInTemplate(template.Slug); ok &&
			template.ScopeKey == "platform" {
			provisioned, err = productCommercialProvisionBuiltInTemplateResources(
				tx,
				organization,
				application,
				*version,
				builtIn,
				userID,
			)
			if err != nil {
				return err
			}
		}

		run := models.ProvisioningRun{
			BaseModel:         models.BaseModel{ID: uuid.New()},
			OrganizationID:    orgID,
			OnboardingID:      onboarding.ID,
			TemplateVersionID: &version.ID,
			Kind:              "template_install",
			IdempotencyKey:    fmt.Sprintf("template:%s:%s", orgID, application.ID),
			Status:            models.ProvisioningStatusSucceeded,
			Attempt:           1,
			Steps: models.JSONBArray{
				map[string]any{"key": "record_template", "status": "succeeded"},
				map[string]any{"key": "set_workspace_vertical", "status": "succeeded"},
				map[string]any{
					"key":              "provision_crm_pipeline",
					"status":           "succeeded",
					"pipeline_created": provisioned.PipelineCreated,
					"stages_created":   provisioned.StagesCreated,
				},
				map[string]any{
					"key":              "provision_booking_services",
					"status":           "succeeded",
					"services_created": provisioned.ServicesCreated,
				},
			},
			RequestedByID: &userID,
			StartedAt:     &now,
			FinishedAt:    &now,
		}
		if err := tx.Create(&run).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.WorkspaceTemplateApplication{}).
			Where("id = ? AND organization_id = ?", application.ID, orgID).
			Update("provisioning_run_id", run.ID).Error; err != nil {
			return err
		}

		resourceMap := models.WorkspaceTemplateResourceMap{
			BaseModel:           models.BaseModel{ID: uuid.New()},
			OrganizationID:      orgID,
			ApplicationID:       application.ID,
			TemplateResourceKey: "workspace.profile.vertical",
			ResourceType:        "organization_setting",
			ResourceID:          orgID.String(),
			Action:              "updated",
			Status:              "active",
			SourceChecksum:      version.Checksum,
			Metadata:            models.JSONB{"vertical": template.Vertical},
		}
		if err := tx.Create(&resourceMap).Error; err != nil {
			return err
		}

		settings := productCommercialJSONCopy(organization.Settings)
		settings["vertical"] = template.Vertical
		settings["workspace_template"] = template.Slug
		settings["workspace_template_version"] = version.Version
		if err := tx.Model(&models.Organization{}).
			Where("id = ?", orgID).
			Update("settings", settings).Error; err != nil {
			return err
		}

		onboarding.TemplateID = &template.ID
		onboarding.TemplateVersionID = &version.ID
		onboarding.Input = productCommercialJSONCopy(onboarding.Input)
		onboarding.Input["vertical"] = template.Vertical
		onboarding.Checklist = productCommercialJSONCopy(onboarding.Checklist)
		onboarding.Checklist["vertical_template"] = true
		onboarding.Checklist["workspace_profile"] = productCommercialProfileComplete(onboarding.Input)
		onboardingResponse, err := productCommercialBuildOnboardingResponse(tx, orgID, onboarding)
		if err != nil {
			return err
		}
		if err := productCommercialPersistOnboardingProgress(tx, onboarding, onboardingResponse); err != nil {
			return err
		}

		if err := audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			productTemplateAuditResource,
			application.ID,
			models.AuditActionCreated,
			nil,
			map[string]any{
				"template_key": template.Slug,
				"version":      version.Version,
				"status":       application.Status,
			},
		); err != nil {
			return err
		}
		response = ApplyWorkspaceTemplateResponse{
			ApplicationID: application.ID,
			TemplateKey:   template.Slug,
			Version:       version.Version,
			Status:        string(application.Status),
		}
		return nil
	})
	if err != nil {
		return a.sendProductCommercialError(r, "apply workspace template", err)
	}
	return r.SendEnvelope(response)
}

func productCommercialValidIdentifier(value string, maxLen int) bool {
	if value == "" || len(value) > maxLen {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') ||
			char == '_' ||
			char == '-' ||
			char == '.' {
			continue
		}
		return false
	}
	return true
}

func productCommercialValidRetentionAction(action models.RetentionAction) bool {
	switch action {
	case models.RetentionActionDelete,
		models.RetentionActionAnonymize,
		models.RetentionActionArchive,
		models.RetentionActionReview:
		return true
	default:
		return false
	}
}

func productCommercialValidateRetentionPolicy(input *RetentionPolicyInput) error {
	input.DataCategory = strings.ToLower(strings.TrimSpace(input.DataCategory))
	input.LegalBasis = strings.TrimSpace(input.LegalBasis)
	if !productCommercialValidIdentifier(input.DataCategory, 100) {
		return errors.New("data_category must use lowercase letters, numbers, dots, dashes, or underscores")
	}
	if input.RetentionDays < 1 || input.RetentionDays > 36500 {
		return errors.New("retention_days must be between 1 and 36500")
	}
	if input.GracePeriodDays < 0 || input.GracePeriodDays > 3650 {
		return errors.New("grace_period_days must be between 0 and 3650")
	}
	if !productCommercialValidRetentionAction(input.Action) {
		return errors.New("action must be delete, anonymize, archive, or review")
	}
	if utf8.RuneCountInString(input.LegalBasis) > 2000 {
		return errors.New("legal_basis must be at most 2000 characters")
	}
	return nil
}

func productCommercialPrivacySettingsResponse(
	policies []models.RetentionPolicy,
) map[string]any {
	response := make([]RetentionPolicyResponse, len(policies))
	for i := range policies {
		response[i] = productCommercialRetentionToResponse(policies[i])
	}
	return map[string]any{"retention_policies": response}
}

// GetPrivacySettings returns tenant retention rules only, never provider data.
func (a *App) GetPrivacySettings(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourcePrivacySettings, models.ActionRead)
	if err != nil {
		return nil
	}
	var policies []models.RetentionPolicy
	if err := a.DB.Where("organization_id = ?", orgID).
		Order("resource_type ASC").
		Find(&policies).Error; err != nil {
		return a.sendProductCommercialError(r, "load privacy settings", err)
	}
	return r.SendEnvelope(productCommercialPrivacySettingsResponse(policies))
}

// UpdatePrivacySettings transactionally upserts supplied retention categories.
func (a *App) UpdatePrivacySettings(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourcePrivacySettings, models.ActionWrite)
	if err != nil {
		return nil
	}
	var req UpdatePrivacySettingsRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if len(req.RetentionPolicies) == 0 {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"At least one retention policy is required",
			nil,
			"",
		)
	}
	if len(req.RetentionPolicies) > 50 {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"At most 50 retention policies may be updated at once",
			nil,
			"",
		)
	}
	seen := make(map[string]struct{}, len(req.RetentionPolicies))
	for i := range req.RetentionPolicies {
		if err := productCommercialValidateRetentionPolicy(&req.RetentionPolicies[i]); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
		}
		if _, duplicate := seen[req.RetentionPolicies[i].DataCategory]; duplicate {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				"Duplicate data_category in retention policies",
				nil,
				"",
			)
		}
		seen[req.RetentionPolicies[i].DataCategory] = struct{}{}
	}

	var policies []models.RetentionPolicy
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		for i := range req.RetentionPolicies {
			input := req.RetentionPolicies[i]
			enabled := true
			if input.IsEnabled != nil {
				enabled = *input.IsEnabled
			}

			var existing models.RetentionPolicy
			findErr := tx.Unscoped().Where(
				"organization_id = ? AND resource_type = ?",
				orgID,
				input.DataCategory,
			).First(&existing).Error
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				policy := models.RetentionPolicy{
					BaseModel:       models.BaseModel{ID: uuid.New()},
					OrganizationID:  orgID,
					ResourceType:    input.DataCategory,
					Action:          input.Action,
					RetentionDays:   input.RetentionDays,
					GracePeriodDays: input.GracePeriodDays,
					LegalBasis:      input.LegalBasis,
					IsActive:        enabled,
					CreatedByID:     userID,
					ApprovedByID:    &userID,
					EffectiveFrom:   now,
				}
				if err := tx.Create(&policy).Error; err != nil {
					return err
				}
				if err := audit.LogAudit(
					tx,
					orgID,
					userID,
					audit.GetUserName(tx, userID),
					productRetentionAuditResource,
					policy.ID,
					models.AuditActionCreated,
					nil,
					productCommercialRetentionToResponse(policy),
				); err != nil {
					return err
				}
				continue
			}
			if findErr != nil {
				return findErr
			}
			oldResponse := productCommercialRetentionToResponse(existing)
			updates := map[string]any{
				"deleted_at":        nil,
				"action":            input.Action,
				"retention_days":    input.RetentionDays,
				"grace_period_days": input.GracePeriodDays,
				"legal_basis":       input.LegalBasis,
				"is_active":         enabled,
				"approved_by_id":    userID,
				"effective_from":    now,
			}
			if err := tx.Unscoped().Model(&models.RetentionPolicy{}).
				Where("id = ? AND organization_id = ?", existing.ID, orgID).
				Updates(updates).Error; err != nil {
				return err
			}
			existing.Action = input.Action
			existing.RetentionDays = input.RetentionDays
			existing.GracePeriodDays = input.GracePeriodDays
			existing.LegalBasis = input.LegalBasis
			existing.IsActive = enabled
			existing.EffectiveFrom = now
			if err := audit.LogAudit(
				tx,
				orgID,
				userID,
				audit.GetUserName(tx, userID),
				productRetentionAuditResource,
				existing.ID,
				models.AuditActionUpdated,
				oldResponse,
				productCommercialRetentionToResponse(existing),
			); err != nil {
				return err
			}
		}
		return tx.Where("organization_id = ?", orgID).
			Order("resource_type ASC").
			Find(&policies).Error
	})
	if err != nil {
		return a.sendProductCommercialError(r, "update privacy settings", err)
	}
	return r.SendEnvelope(productCommercialPrivacySettingsResponse(policies))
}

func productCommercialValidConsentAction(action models.ConsentAction) bool {
	switch action {
	case models.ConsentActionGranted,
		models.ConsentActionDenied,
		models.ConsentActionWithdrawn,
		models.ConsentActionExpired:
		return true
	default:
		return false
	}
}

func productCommercialConsentStatus(action models.ConsentAction) models.ConsentStatus {
	switch action {
	case models.ConsentActionGranted:
		return models.ConsentStatusGranted
	case models.ConsentActionDenied:
		return models.ConsentStatusDenied
	case models.ConsentActionWithdrawn:
		return models.ConsentStatusWithdrawn
	case models.ConsentActionExpired:
		return models.ConsentStatusExpired
	default:
		return models.ConsentStatusUnknown
	}
}

func productCommercialValidateConsent(req *RecordConsentRequest) ([]byte, error) {
	req.SubjectType = strings.ToLower(strings.TrimSpace(req.SubjectType))
	req.SubjectKey = strings.TrimSpace(req.SubjectKey)
	req.Purpose = strings.ToLower(strings.TrimSpace(req.Purpose))
	req.Channel = strings.ToLower(strings.TrimSpace(req.Channel))
	req.LegalBasis = strings.TrimSpace(req.LegalBasis)
	req.PolicyVersion = strings.TrimSpace(req.PolicyVersion)
	req.Source = strings.ToLower(strings.TrimSpace(req.Source))
	if req.SubjectType == "" {
		req.SubjectType = "contact"
	}
	if req.Source == "" {
		req.Source = "manual"
	}
	if !productCommercialValidIdentifier(req.SubjectType, 50) {
		return nil, errors.New("invalid subject_type")
	}
	if req.SubjectKey == "" && (req.ContactID == nil || *req.ContactID == uuid.Nil) {
		return nil, errors.New("contact_id or subject_key is required")
	}
	if len(req.SubjectKey) > 255 {
		return nil, errors.New("subject_key must be at most 255 characters")
	}
	if !productCommercialValidIdentifier(req.Purpose, 100) {
		return nil, errors.New("invalid consent purpose")
	}
	if !productCommercialValidIdentifier(req.Channel, 50) {
		return nil, errors.New("invalid consent channel")
	}
	if !productCommercialValidConsentAction(req.Action) {
		return nil, errors.New("invalid consent action")
	}
	if utf8.RuneCountInString(req.LegalBasis) > 1000 {
		return nil, errors.New("legal_basis must be at most 1000 characters")
	}
	if len(req.PolicyVersion) > 100 {
		return nil, errors.New("policy_version must be at most 100 characters")
	}
	if err := productCommercialRejectSensitiveJSON(req.Evidence, "evidence"); err != nil {
		return nil, err
	}
	evidenceBytes, err := json.Marshal(req.Evidence)
	if err != nil {
		return nil, errors.New("evidence must contain valid JSON values")
	}
	if len(evidenceBytes) > productCommercialMaxEvidenceBytes {
		return nil, fmt.Errorf("evidence must be at most %d bytes", productCommercialMaxEvidenceBytes)
	}
	return evidenceBytes, nil
}

// ListConsentStates returns materialized consent state, never raw evidence.
func (a *App) ListConsentStates(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourcePrivacyConsents, models.ActionRead)
	if err != nil {
		return nil
	}
	pg := parsePagination(r)
	query := a.DB.Model(&models.ConsentState{}).Where("organization_id = ?", orgID)
	if rawContactID := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("contact_id"))); rawContactID != "" {
		contactID, err := uuid.Parse(rawContactID)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid contact_id", nil, "")
		}
		query = query.Where("contact_id = ?", contactID)
	}
	for field, maxLen := range map[string]int{
		"purpose": 100,
		"channel": 50,
		"status":  20,
	} {
		value := strings.ToLower(strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek(field))))
		if value == "" {
			continue
		}
		if !productCommercialValidIdentifier(value, maxLen) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid "+field, nil, "")
		}
		query = query.Where(field+" = ?", value)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return a.sendProductCommercialError(r, "count consent states", err)
	}
	var states []models.ConsentState
	if err := pg.Apply(query).
		Order("effective_at DESC, created_at DESC").
		Find(&states).Error; err != nil {
		return a.sendProductCommercialError(r, "list consent states", err)
	}
	response := make([]ConsentStateResponse, len(states))
	for i := range states {
		response[i] = productCommercialConsentStateToResponse(states[i])
	}
	return r.SendEnvelope(listEnvelope("consents", response, total, pg))
}

func productCommercialRecordConsentTx(
	tx *gorm.DB,
	event *models.ConsentEvent,
	state *models.ConsentState,
	userID uuid.UUID,
) error {
	if tx == nil || event == nil || state == nil {
		return errors.New("complete consent transaction state is required")
	}
	if event.ContactID != nil {
		if err := database.LockContactPolicyScope(
			tx,
			event.OrganizationID,
			*event.ContactID,
		); err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return newProductCommercialClientError(
					fasthttp.StatusBadRequest,
					"contact_id does not belong to the organization",
				)
			}
			return fmt.Errorf("lock consent policy scope: %w", err)
		}
	}
	if err := tx.Create(event).Error; err != nil {
		return err
	}
	if err := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "organization_id"},
			{Name: "subject_type"},
			{Name: "subject_key"},
			{Name: "purpose"},
			{Name: "channel"},
		},
		DoUpdates: clause.AssignmentColumns([]string{
			"contact_id",
			"status",
			"latest_event_id",
			"policy_version",
			"effective_at",
			"expires_at",
			"metadata",
			"updated_at",
		}),
	}).Create(state).Error; err != nil {
		return err
	}
	if err := tx.Where(
		"organization_id = ? AND subject_type = ? AND subject_key = ? AND purpose = ? AND channel = ?",
		event.OrganizationID,
		event.SubjectType,
		event.SubjectKey,
		event.Purpose,
		event.Channel,
	).First(state).Error; err != nil {
		return err
	}
	return audit.LogAudit(
		tx,
		event.OrganizationID,
		userID,
		audit.GetUserName(tx, userID),
		productConsentAuditResource,
		event.ID,
		models.AuditActionCreated,
		nil,
		map[string]any{
			"subject_type":   event.SubjectType,
			"purpose":        event.Purpose,
			"channel":        event.Channel,
			"action":         event.Action,
			"policy_version": event.PolicyVersion,
			"evidence_hash":  event.EvidenceHash,
		},
	)
}

// RecordConsent appends evidence and atomically upserts the effective state.
// Contact-scoped changes share a row lock with the automatic-reply dispatch
// fence so a committed withdrawal cannot race past provider delivery.
func (a *App) RecordConsent(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourcePrivacyConsents, models.ActionWrite)
	if err != nil {
		return nil
	}
	var req RecordConsentRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	evidenceBytes, err := productCommercialValidateConsent(&req)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	if req.ContactID != nil {
		if *req.ContactID == uuid.Nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid contact_id", nil, "")
		}
	}
	subjectKeyFollowsContact := req.ContactID != nil &&
		(req.SubjectKey == "" ||
			(req.SubjectType == "contact" && req.SubjectKey == req.ContactID.String()))

	now := time.Now().UTC()
	hash := sha256.Sum256(evidenceBytes)
	var event models.ConsentEvent
	var state models.ConsentState
	err = canonicalContactWriteTransaction(a.DB, func(tx *gorm.DB) error {
		if req.ContactID != nil {
			canonical, resolveErr := contactutil.ResolveCanonicalContactForUpdate(
				tx,
				orgID,
				*req.ContactID,
			)
			if resolveErr != nil {
				return resolveErr
			}
			canonicalID := canonical.ID
			req.ContactID = &canonicalID
			if subjectKeyFollowsContact {
				req.SubjectKey = canonicalID.String()
			}
		}

		event = models.ConsentEvent{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: orgID,
			ContactID:      req.ContactID,
			SubjectType:    req.SubjectType,
			SubjectKey:     req.SubjectKey,
			Purpose:        req.Purpose,
			Channel:        req.Channel,
			Action:         req.Action,
			LegalBasis:     req.LegalBasis,
			PolicyVersion:  req.PolicyVersion,
			Source:         req.Source,
			ActorUserID:    &userID,
			Evidence:       productCommercialJSONCopy(req.Evidence),
			EvidenceHash:   fmt.Sprintf("%x", hash),
			CapturedAt:     now,
			ExpiresAt:      req.ExpiresAt,
		}
		state = models.ConsentState{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: orgID,
			ContactID:      req.ContactID,
			SubjectType:    req.SubjectType,
			SubjectKey:     req.SubjectKey,
			Purpose:        req.Purpose,
			Channel:        req.Channel,
			Status:         productCommercialConsentStatus(req.Action),
			LatestEventID:  event.ID,
			PolicyVersion:  req.PolicyVersion,
			EffectiveAt:    now,
			ExpiresAt:      req.ExpiresAt,
			Metadata:       models.JSONB{},
		}
		return productCommercialRecordConsentTx(tx, &event, &state, userID)
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) && req.ContactID != nil {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				"contact_id does not belong to the organization",
				nil,
				"",
			)
		}
		return a.sendProductCommercialError(r, "record consent", err)
	}
	return r.SendEnvelope(map[string]any{
		"event_id": event.ID,
		"consent":  productCommercialConsentStateToResponse(state),
	})
}

func productCommercialValidPrivacyRequestType(requestType models.PrivacyRequestType) bool {
	switch requestType {
	case models.PrivacyRequestTypeAccess,
		models.PrivacyRequestTypeExport,
		models.PrivacyRequestTypeCorrection,
		models.PrivacyRequestTypeDeletion,
		models.PrivacyRequestTypeErasure,
		models.PrivacyRequestTypeRestriction,
		models.PrivacyRequestTypePortability,
		models.PrivacyRequestTypeObjection:
		return true
	default:
		return false
	}
}

func productCommercialValidPrivacyRequestStatus(status models.PrivacyRequestStatus) bool {
	switch status {
	case models.PrivacyRequestStatusReceived,
		models.PrivacyRequestStatusAwaitingVerification,
		models.PrivacyRequestStatusVerified,
		models.PrivacyRequestStatusInProgress,
		models.PrivacyRequestStatusCompleted,
		models.PrivacyRequestStatusDenied,
		models.PrivacyRequestStatusCanceled,
		models.PrivacyRequestStatusExpired:
		return true
	default:
		return false
	}
}

func productCommercialPrivacyTransitionAllowed(
	from, to models.PrivacyRequestStatus,
) bool {
	if from == to {
		return true
	}
	allowed := map[models.PrivacyRequestStatus]map[models.PrivacyRequestStatus]bool{
		models.PrivacyRequestStatusReceived: {
			models.PrivacyRequestStatusAwaitingVerification: true,
			models.PrivacyRequestStatusVerified:             true,
			models.PrivacyRequestStatusInProgress:           true,
			models.PrivacyRequestStatusDenied:               true,
			models.PrivacyRequestStatusCanceled:             true,
		},
		models.PrivacyRequestStatusAwaitingVerification: {
			models.PrivacyRequestStatusVerified: true,
			models.PrivacyRequestStatusDenied:   true,
			models.PrivacyRequestStatusCanceled: true,
			models.PrivacyRequestStatusExpired:  true,
		},
		models.PrivacyRequestStatusVerified: {
			models.PrivacyRequestStatusInProgress: true,
			models.PrivacyRequestStatusCompleted:  true,
			models.PrivacyRequestStatusDenied:     true,
			models.PrivacyRequestStatusCanceled:   true,
		},
		models.PrivacyRequestStatusInProgress: {
			models.PrivacyRequestStatusCompleted: true,
			models.PrivacyRequestStatusDenied:    true,
			models.PrivacyRequestStatusCanceled:  true,
		},
	}
	return allowed[from][to]
}

func productCommercialPrivacyCompletionHasEvidence(current string, update *string) bool {
	effective := current
	if update != nil {
		effective = *update
	}
	return strings.TrimSpace(effective) != ""
}

func productCommercialValidatePrivateJSON(value models.JSONB, name string) error {
	if value == nil {
		return nil
	}
	if err := productCommercialRejectSensitiveJSON(value, name); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%s must contain valid JSON values", name)
	}
	if len(data) > productCommercialMaxEvidenceBytes {
		return fmt.Errorf("%s must be at most %d bytes", name, productCommercialMaxEvidenceBytes)
	}
	return nil
}

// ListPrivacyRequests returns paginated tenant requests without requester PII.
func (a *App) ListPrivacyRequests(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourcePrivacyRequests, models.ActionRead)
	if err != nil {
		return nil
	}
	pg := parsePagination(r)
	query := a.DB.Model(&models.PrivacyRequest{}).Where("organization_id = ?", orgID)
	if rawStatus := strings.ToLower(strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("status")))); rawStatus != "" {
		status := models.PrivacyRequestStatus(rawStatus)
		if !productCommercialValidPrivacyRequestStatus(status) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid privacy request status", nil, "")
		}
		query = query.Where("status = ?", status)
	}
	if rawType := strings.ToLower(strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("request_type")))); rawType != "" {
		requestType := models.PrivacyRequestType(rawType)
		if !productCommercialValidPrivacyRequestType(requestType) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid privacy request type", nil, "")
		}
		query = query.Where("type = ?", requestType)
	}
	if rawContactID := strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("contact_id"))); rawContactID != "" {
		contactID, err := uuid.Parse(rawContactID)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid contact_id", nil, "")
		}
		query = query.Where("contact_id = ?", contactID)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return a.sendProductCommercialError(r, "count privacy requests", err)
	}
	var requests []models.PrivacyRequest
	if err := pg.Apply(query).
		Order("created_at DESC").
		Find(&requests).Error; err != nil {
		return a.sendProductCommercialError(r, "list privacy requests", err)
	}
	response := make([]PrivacyRequestResponse, len(requests))
	for i := range requests {
		response[i] = productCommercialPrivacyRequestToResponse(requests[i])
	}
	return r.SendEnvelope(listEnvelope("requests", response, total, pg))
}

// CreatePrivacyRequest opens a request in identity-verification state.
func (a *App) CreatePrivacyRequest(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourcePrivacyRequests, models.ActionWrite)
	if err != nil {
		return nil
	}
	var req CreatePrivacyRequestRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	req.SubjectType = strings.ToLower(strings.TrimSpace(req.SubjectType))
	req.SubjectKey = strings.TrimSpace(req.SubjectKey)
	req.ReceivedChannel = strings.ToLower(strings.TrimSpace(req.ReceivedChannel))
	if req.SubjectType == "" {
		req.SubjectType = "contact"
	}
	if req.ReceivedChannel == "" {
		req.ReceivedChannel = "internal"
	}
	if !productCommercialValidPrivacyRequestType(req.RequestType) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid privacy request type", nil, "")
	}
	if !productCommercialValidIdentifier(req.SubjectType, 50) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid subject_type", nil, "")
	}
	if len(req.SubjectKey) > 255 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "subject_key must be at most 255 characters", nil, "")
	}
	if !productCommercialValidIdentifier(req.ReceivedChannel, 50) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid received_channel", nil, "")
	}
	if err := productCommercialValidatePrivateJSON(req.RequesterProfile, "requester_profile"); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	if err := productCommercialValidatePrivateJSON(req.Details, "details"); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	if req.ContactID != nil {
		if *req.ContactID == uuid.Nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid contact_id", nil, "")
		}
		var count int64
		if err := a.DB.Model(&models.Contact{}).
			Where("id = ? AND organization_id = ?", *req.ContactID, orgID).
			Count(&count).Error; err != nil {
			return a.sendProductCommercialError(r, "validate privacy request contact", err)
		}
		if count == 0 {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				"contact_id does not belong to the organization",
				nil,
				"",
			)
		}
		if req.SubjectKey == "" {
			req.SubjectKey = req.ContactID.String()
		}
	}

	now := time.Now().UTC()
	requestID := uuid.New()
	if req.SubjectKey == "" {
		req.SubjectKey = "unverified:" + requestID.String()
	}
	request := models.PrivacyRequest{
		BaseModel:        models.BaseModel{ID: requestID},
		OrganizationID:   orgID,
		RequestNumber:    fmt.Sprintf("PR-%s-%s", now.Format("20060102"), strings.ToUpper(requestID.String()[:8])),
		Type:             req.RequestType,
		Status:           models.PrivacyRequestStatusAwaitingVerification,
		SubjectType:      req.SubjectType,
		SubjectKey:       req.SubjectKey,
		ContactID:        req.ContactID,
		ReceivedChannel:  req.ReceivedChannel,
		RequesterProfile: productCommercialJSONCopy(req.RequesterProfile),
		RequestDetails:   productCommercialJSONCopy(req.Details),
		ReceivedAt:       now,
		DueAt:            now.Add(30 * 24 * time.Hour),
	}
	event := models.PrivacyRequestEvent{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   orgID,
		PrivacyRequestID: request.ID,
		EventType:        "request_created",
		ToStatus:         request.Status,
		ActorUserID:      &userID,
		Message:          "Privacy request opened; identity verification required",
		Details:          models.JSONB{},
		OccurredAt:       now,
	}
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&request).Error; err != nil {
			return err
		}
		if err := tx.Create(&event).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			productPrivacyRequestAuditResource,
			request.ID,
			models.AuditActionCreated,
			nil,
			productCommercialPrivacyRequestToResponse(request),
		)
	})
	if err != nil {
		return a.sendProductCommercialError(r, "create privacy request", err)
	}
	return r.SendEnvelope(productCommercialPrivacyRequestToResponse(request))
}

// UpdatePrivacyRequest performs an audited, validated workflow transition.
func (a *App) UpdatePrivacyRequest(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourcePrivacyRequests, models.ActionWrite)
	if err != nil {
		return nil
	}
	requestID, err := parsePathUUID(r, "id", "privacy request")
	if err != nil {
		return nil
	}
	var req UpdatePrivacyRequestRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if req.Status == nil &&
		req.AssignedUserID == nil &&
		!req.ClearAssignee &&
		req.Resolution == nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "No privacy request changes supplied", nil, "")
	}
	if req.Status != nil && !productCommercialValidPrivacyRequestStatus(*req.Status) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid privacy request status", nil, "")
	}
	if req.Resolution != nil {
		trimmed := strings.TrimSpace(*req.Resolution)
		if utf8.RuneCountInString(trimmed) > 4000 {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				"resolution must be at most 4000 characters",
				nil,
				"",
			)
		}
		req.Resolution = &trimmed
	}
	if req.AssignedUserID != nil {
		belongs, err := productCommercialUserBelongsToOrganization(a.DB, orgID, *req.AssignedUserID)
		if err != nil {
			return a.sendProductCommercialError(r, "validate privacy request assignee", err)
		}
		if !belongs {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				"assigned_user_id does not belong to the organization",
				nil,
				"",
			)
		}
	}

	var updated models.PrivacyRequest
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var request models.PrivacyRequest
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", requestID, orgID).
			First(&request).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return newProductCommercialClientError(fasthttp.StatusNotFound, "Privacy request not found")
			}
			return err
		}
		oldResponse := productCommercialPrivacyRequestToResponse(request)
		updates := map[string]any{}
		fromStatus := request.Status
		toStatus := request.Status
		if req.Status != nil {
			toStatus = *req.Status
			if !productCommercialPrivacyTransitionAllowed(fromStatus, toStatus) {
				return newProductCommercialClientError(
					fasthttp.StatusConflict,
					"Invalid privacy request status transition",
				)
			}
			if toStatus == models.PrivacyRequestStatusCompleted &&
				!productCommercialPrivacyCompletionHasEvidence(request.DecisionReason, req.Resolution) {
				return newProductCommercialClientError(
					fasthttp.StatusConflict,
					"Completion requires documented fulfillment evidence in resolution",
				)
			}
			updates["status"] = toStatus
			request.Status = toStatus
			now := time.Now().UTC()
			if toStatus == models.PrivacyRequestStatusVerified && request.VerifiedAt == nil {
				updates["verified_at"] = now
				request.VerifiedAt = &now
			}
			if toStatus == models.PrivacyRequestStatusCompleted && request.CompletedAt == nil {
				updates["completed_at"] = now
				request.CompletedAt = &now
			}
		}
		if req.ClearAssignee {
			updates["assigned_to_id"] = nil
			request.AssignedToID = nil
		} else if req.AssignedUserID != nil {
			updates["assigned_to_id"] = *req.AssignedUserID
			request.AssignedToID = req.AssignedUserID
		}
		if req.Resolution != nil {
			updates["decision_reason"] = *req.Resolution
			request.DecisionReason = *req.Resolution
		}
		if len(updates) == 0 {
			updated = request
			return nil
		}
		if err := tx.Model(&models.PrivacyRequest{}).
			Where("id = ? AND organization_id = ?", request.ID, orgID).
			Updates(updates).Error; err != nil {
			return err
		}
		eventType := "request_updated"
		if fromStatus != toStatus {
			eventType = "status_changed"
		}
		event := models.PrivacyRequestEvent{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			OrganizationID:   orgID,
			PrivacyRequestID: request.ID,
			EventType:        eventType,
			FromStatus:       fromStatus,
			ToStatus:         toStatus,
			ActorUserID:      &userID,
			Message:          "Privacy request updated",
			Details:          models.JSONB{},
			OccurredAt:       time.Now().UTC(),
		}
		if err := tx.Create(&event).Error; err != nil {
			return err
		}
		if err := audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			productPrivacyRequestAuditResource,
			request.ID,
			models.AuditActionUpdated,
			oldResponse,
			productCommercialPrivacyRequestToResponse(request),
		); err != nil {
			return err
		}
		updated = request
		return nil
	})
	if err != nil {
		return a.sendProductCommercialError(r, "update privacy request", err)
	}
	return r.SendEnvelope(productCommercialPrivacyRequestToResponse(updated))
}

func productCommercialHealthStatus(checks []TenantHealthCheckResponse) string {
	status := "healthy"
	for _, check := range checks {
		if check.Status == "fail" {
			return "degraded"
		}
		if check.Status != "pass" {
			status = "attention"
		}
	}
	return status
}

// GetTenantSupportHealth runs organization-scoped, read-only checks. Database
// error details are logged server-side and never returned to the client.
func (a *App) GetTrustQueueSummary(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	response := TrustQueueSummaryResponse{
		PrivacyVisible: a.HasPermission(
			userID,
			models.ResourcePrivacyRequests,
			models.ActionRead,
			orgID,
		),
		SupportVisible: a.HasPermission(
			userID,
			models.ResourceSupport,
			models.ActionRead,
			orgID,
		),
	}
	if !response.PrivacyVisible && !response.SupportVisible {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Insufficient permissions", nil, "")
	}
	if response.PrivacyVisible {
		if err := a.DB.Model(&models.PrivacyRequest{}).
			Where(
				"organization_id = ? AND status NOT IN ?",
				orgID,
				[]models.PrivacyRequestStatus{
					models.PrivacyRequestStatusCompleted,
					models.PrivacyRequestStatusDenied,
					models.PrivacyRequestStatusCanceled,
					models.PrivacyRequestStatusExpired,
				},
			).
			Count(&response.PrivacyOpen).Error; err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to summarize privacy requests", nil, "")
		}
	}
	if response.SupportVisible {
		if err := a.DB.Model(&models.SupportCase{}).
			Where(
				"organization_id = ? AND status NOT IN ?",
				orgID,
				[]models.SupportCaseStatus{
					models.SupportCaseStatusResolved,
					models.SupportCaseStatusClosed,
				},
			).
			Count(&response.SupportOpen).Error; err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to summarize support cases", nil, "")
		}
	}
	return r.SendEnvelope(response)
}

func (a *App) GetTenantSupportHealth(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceSupport, models.ActionRead)
	if err != nil {
		return nil
	}
	checks := make([]TenantHealthCheckResponse, 0, 7)
	healthNow := time.Now().UTC()

	var organizationCount int64
	dbErr := a.DB.Model(&models.Organization{}).
		Where("id = ?", orgID).
		Count(&organizationCount).Error
	if dbErr != nil || organizationCount != 1 {
		if dbErr != nil {
			a.Log.Error("Tenant health database check failed", "error", dbErr, "organization_id", orgID)
		}
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "database",
			Label:  "Tenant database",
			Status: "fail",
			Detail: "The organization workspace could not be verified.",
		})
	} else {
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "database",
			Label:  "Tenant database",
			Status: "pass",
			Detail: "The organization workspace is reachable and tenant-bound.",
		})
	}

	var channelCount int64
	var nonLegacyChannelCount int64
	var nonWhatsAppChannelCount int64
	var staleChannelCount int64
	var errorChannelCount int64
	var retryingOutboxCount int64
	var failedOutboxCount int64
	var staleOutboxCount int64
	var intendedChannels []models.Channel
	var missingIntendedChannels []string
	intendedChannelsDeclared := false
	channelFailed := false
	if a.DB.Migrator().HasTable("whatsapp_accounts") {
		if err := a.DB.Table("whatsapp_accounts").
			Where("organization_id = ? AND status = ? AND deleted_at IS NULL", orgID, "active").
			Count(&channelCount).Error; err != nil {
			a.Log.Error("Tenant health WhatsApp check failed", "error", err, "organization_id", orgID)
			channelFailed = true
		}
	}
	if a.DB.Migrator().HasTable("channel_accounts") {
		if err := a.DB.Table("channel_accounts").
			Where(
				"organization_id = ? AND status = ? AND deleted_at IS NULL AND provider <> ? AND LOWER(COALESCE(config->>'outbound_enabled', 'false')) = 'true'",
				orgID,
				"active",
				"meta_legacy",
			).
			Count(&nonLegacyChannelCount).Error; err != nil {
			a.Log.Error("Tenant health channel check failed", "error", err, "organization_id", orgID)
			channelFailed = true
		}
		channelCount += nonLegacyChannelCount
		if err := a.DB.Table("channel_accounts").
			Where(
				"organization_id = ? AND status = ? AND deleted_at IS NULL AND provider <> ? AND channel <> ? AND LOWER(COALESCE(config->>'outbound_enabled', 'false')) = 'true'",
				orgID,
				"active",
				"meta_legacy",
				models.ChannelWhatsApp,
			).
			Count(&nonWhatsAppChannelCount).Error; err != nil {
			a.Log.Error("Tenant health non-WhatsApp channel check failed", "error", err, "organization_id", orgID)
			channelFailed = true
		}
		readyRelay := a.DB.Table("channel_accounts").Where(
			"organization_id = ? AND status = ? AND deleted_at IS NULL AND provider <> ? AND LOWER(COALESCE(config->>'outbound_enabled', 'false')) = 'true'",
			orgID,
			"active",
			"meta_legacy",
		)
		if err := readyRelay.
			Where(
				"last_health_check_at IS NULL OR last_health_check_at < ?",
				healthNow.Add(-24*time.Hour),
			).
			Count(&staleChannelCount).Error; err != nil {
			a.Log.Error("Tenant health stale channel check failed", "error", err, "organization_id", orgID)
			channelFailed = true
		}
		if err := a.DB.Table("channel_accounts").
			Where(
				"organization_id = ? AND deleted_at IS NULL AND provider <> ? AND (status = ? OR (last_error_at IS NOT NULL AND last_error <> ''))",
				orgID,
				"meta_legacy",
				"degraded",
			).
			Count(&errorChannelCount).Error; err != nil {
			a.Log.Error("Tenant health channel error check failed", "error", err, "organization_id", orgID)
			channelFailed = true
		}
	}
	if a.DB.Migrator().HasTable("outbox_jobs") {
		if err := a.DB.Table("outbox_jobs").
			Where("organization_id = ? AND status = ?", orgID, models.OutboxJobStatusRetrying).
			Count(&retryingOutboxCount).Error; err != nil {
			a.Log.Error("Tenant health retrying outbox check failed", "error", err, "organization_id", orgID)
			channelFailed = true
		}
		if err := a.DB.Table("outbox_jobs").
			Where("organization_id = ? AND status = ?", orgID, models.OutboxJobStatusFailed).
			Count(&failedOutboxCount).Error; err != nil {
			a.Log.Error("Tenant health failed outbox check failed", "error", err, "organization_id", orgID)
			channelFailed = true
		}
		if err := a.DB.Table("outbox_jobs").
			Where(
				"organization_id = ? AND status IN ? AND available_at < ?",
				orgID,
				[]models.OutboxJobStatus{
					models.OutboxJobStatusPending,
					models.OutboxJobStatusRetrying,
				},
				healthNow.Add(-10*time.Minute),
			).
			Count(&staleOutboxCount).Error; err != nil {
			a.Log.Error("Tenant health stale outbox check failed", "error", err, "organization_id", orgID)
			channelFailed = true
		}
	}
	if a.DB.Migrator().HasTable("organization_onboardings") {
		var intendedErr error
		intendedChannels, intendedChannelsDeclared, intendedErr =
			productCommercialWorkspaceIntendedChannels(a.DB, orgID)
		if intendedErr != nil {
			a.Log.Error(
				"Tenant health intended channel check failed",
				"error", intendedErr,
				"organization_id", orgID,
			)
			channelFailed = true
		} else if intendedChannelsDeclared && len(intendedChannels) > 0 {
			readyChannels, readyErr := productCommercialReadyChannels(a.DB, orgID)
			if readyErr != nil {
				a.Log.Error(
					"Tenant health intended channel readiness failed",
					"error", readyErr,
					"organization_id", orgID,
				)
				channelFailed = true
			} else {
				for _, channel := range intendedChannels {
					if !readyChannels[channel] {
						missingIntendedChannels = append(
							missingIntendedChannels,
							productCommercialChannelLabel(channel),
						)
					}
				}
			}
		}
	}
	omnichannelEnabled, entitlementErr := a.HasProductEntitlement(
		uuid.Nil,
		orgID,
		"omnichannel.enabled",
	)
	if entitlementErr != nil {
		a.Log.Error(
			"Tenant health omnichannel entitlement check failed",
			"error", entitlementErr,
			"organization_id", orgID,
		)
		channelFailed = true
	}
	switch {
	case channelFailed:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "channels",
			Label:  "Customer channels",
			Status: "fail",
			Detail: "Channel readiness could not be verified.",
		})
	case errorChannelCount > 0 || failedOutboxCount > 0:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "channels",
			Label:  "Customer channels",
			Status: "fail",
			Detail: fmt.Sprintf(
				"%d channel connection(s) report an error and %d outbound job(s) are dead-lettered.",
				errorChannelCount,
				failedOutboxCount,
			),
		})
	case staleChannelCount > 0 || retryingOutboxCount > 0 || staleOutboxCount > 0:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "channels",
			Label:  "Customer channels",
			Status: "warn",
			Detail: fmt.Sprintf(
				"%d active channel connection(s) need a fresh health test, %d outbound job(s) are retrying, and %d job(s) are more than 10 minutes overdue.",
				staleChannelCount,
				retryingOutboxCount,
				staleOutboxCount,
			),
		})
	case intendedChannelsDeclared && len(intendedChannels) == 0:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "channels",
			Label:  "Customer channels",
			Status: "warn",
			Detail: "Select at least one intended launch channel in the workspace profile, then authorize and test it.",
		})
	case len(missingIntendedChannels) > 0:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "channels",
			Label:  "Customer channels",
			Status: "warn",
			Detail: fmt.Sprintf(
				"Intended launch channels are not ready: %s. Authorize and test each one before go-live.",
				strings.Join(missingIntendedChannels, ", "),
			),
		})
	case channelCount == 0:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "channels",
			Label:  "Customer channels",
			Status: "not_configured",
			Detail: "No active customer channel is connected.",
		})
	case !intendedChannelsDeclared && omnichannelEnabled && nonWhatsAppChannelCount == 0:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "channels",
			Label:  "Customer channels",
			Status: "warn",
			Detail: "WhatsApp is active, but this omnichannel workspace has no tested Instagram, Messenger, Threads, email, or web chat connection.",
		})
	default:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "channels",
			Label:  "Customer channels",
			Status: "pass",
			Detail: fmt.Sprintf("%d active channel connection(s) are available.", channelCount),
		})
	}

	var subscription models.Subscription
	subscriptionErr := productCommercialLoadCurrentSubscription(a.DB, orgID, &subscription)
	switch {
	case errors.Is(subscriptionErr, gorm.ErrRecordNotFound):
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "subscription",
			Label:  "Commercial license",
			Status: "fail",
			Detail: "No commercial subscription is assigned. Product modules remain locked until a plan is activated.",
		})
	case subscriptionErr != nil:
		a.Log.Error("Tenant health subscription check failed", "error", subscriptionErr, "organization_id", orgID)
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "subscription",
			Label:  "Commercial license",
			Status: "fail",
			Detail: "The commercial license could not be verified.",
		})
	case productCommercialSubscriptionPermitsFeatures(&subscription, time.Now().UTC()):
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "subscription",
			Label:  "Commercial license",
			Status: "pass",
			Detail: "The workspace subscription is active.",
		})
	default:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "subscription",
			Label:  "Commercial license",
			Status: "warn",
			Detail: "The workspace subscription requires attention.",
		})
	}

	var failedPaymentCount int64
	var failedPaymentWebhookCount int64
	paymentFailed := false
	if a.DB.Migrator().HasTable("payment_transactions") {
		if err := a.DB.Model(&models.PaymentTransaction{}).
			Where(
				"organization_id = ? AND status = ? AND occurred_at >= ?",
				orgID,
				models.PaymentTransactionStatusFailed,
				healthNow.Add(-24*time.Hour),
			).
			Count(&failedPaymentCount).Error; err != nil {
			a.Log.Error("Tenant health payment failure check failed", "error", err, "organization_id", orgID)
			paymentFailed = true
		}
	}
	if a.DB.Migrator().HasTable("payment_webhook_events") {
		if err := a.DB.Model(&models.PaymentWebhookEvent{}).
			Where(
				"organization_id = ? AND status = ? AND received_at >= ?",
				orgID,
				models.PaymentWebhookEventStatusFailed,
				healthNow.Add(-24*time.Hour),
			).
			Count(&failedPaymentWebhookCount).Error; err != nil {
			a.Log.Error("Tenant health payment webhook check failed", "error", err, "organization_id", orgID)
			paymentFailed = true
		}
	}
	switch {
	case paymentFailed:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "payments",
			Label:  "Payment processing",
			Status: "fail",
			Detail: "Recent payment processing health could not be verified.",
		})
	case failedPaymentCount > 0 || failedPaymentWebhookCount > 0:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "payments",
			Label:  "Payment processing",
			Status: "warn",
			Detail: fmt.Sprintf(
				"%d failed payment transaction(s) and %d failed provider webhook(s) were recorded in the last 24 hours.",
				failedPaymentCount,
				failedPaymentWebhookCount,
			),
		})
	default:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "payments",
			Label:  "Payment processing",
			Status: "pass",
			Detail: "No failed payment transaction or provider webhook was recorded in the last 24 hours.",
		})
	}

	var retentionCount int64
	var overduePrivacyCount int64
	retentionErr := a.DB.Model(&models.RetentionPolicy{}).
		Where("organization_id = ? AND is_active = ?", orgID, true).
		Count(&retentionCount).Error
	if retentionErr == nil {
		retentionErr = a.DB.Model(&models.PrivacyRequest{}).
			Where(
				"organization_id = ? AND due_at < ? AND status NOT IN ?",
				orgID,
				healthNow,
				[]models.PrivacyRequestStatus{
					models.PrivacyRequestStatusCompleted,
					models.PrivacyRequestStatusDenied,
					models.PrivacyRequestStatusCanceled,
					models.PrivacyRequestStatusExpired,
				},
			).
			Count(&overduePrivacyCount).Error
	}
	switch {
	case retentionErr != nil:
		a.Log.Error("Tenant health retention check failed", "error", retentionErr, "organization_id", orgID)
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "privacy",
			Label:  "Privacy baseline",
			Status: "fail",
			Detail: "Retention controls could not be verified.",
		})
	case overduePrivacyCount > 0:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "privacy",
			Label:  "Privacy baseline",
			Status: "fail",
			Detail: fmt.Sprintf("%d privacy request(s) are past their due date.", overduePrivacyCount),
		})
	case retentionCount == 0:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "privacy",
			Label:  "Privacy baseline",
			Status: "not_configured",
			Detail: "No active retention policy is documented.",
		})
	default:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "privacy",
			Label:  "Privacy baseline",
			Status: "pass",
			Detail: fmt.Sprintf("%d active retention rule(s) are documented.", retentionCount),
		})
	}

	var checkpointCount int64
	checkpointErr := a.DB.Model(&models.RecoveryCheckpoint{}).
		Where(
			"organization_id = ? AND status = ? AND verified_at IS NOT NULL AND (expires_at IS NULL OR expires_at > ?)",
			orgID,
			models.RecoveryCheckpointStatusReady,
			healthNow,
		).
		Count(&checkpointCount).Error
	switch {
	case checkpointErr != nil:
		a.Log.Error("Tenant health recovery check failed", "error", checkpointErr, "organization_id", orgID)
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "recovery",
			Label:  "Recovery readiness",
			Status: "fail",
			Detail: "Recovery readiness could not be verified.",
		})
	case checkpointCount == 0:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "recovery",
			Label:  "Recovery readiness",
			Status: "not_configured",
			Detail: "No current verified recovery checkpoint is recorded.",
		})
	default:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "recovery",
			Label:  "Recovery readiness",
			Status: "pass",
			Detail: "A current verified recovery checkpoint is recorded.",
		})
	}

	var criticalCaseCount int64
	caseErr := a.DB.Model(&models.SupportCase{}).
		Where(
			"organization_id = ? AND priority = ? AND status NOT IN ?",
			orgID,
			"critical",
			[]models.SupportCaseStatus{
				models.SupportCaseStatusResolved,
				models.SupportCaseStatusClosed,
			},
		).
		Count(&criticalCaseCount).Error
	switch {
	case caseErr != nil:
		a.Log.Error("Tenant health support check failed", "error", caseErr, "organization_id", orgID)
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "support",
			Label:  "Critical support queue",
			Status: "fail",
			Detail: "The support queue could not be verified.",
		})
	case criticalCaseCount > 0:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "support",
			Label:  "Critical support queue",
			Status: "warn",
			Detail: fmt.Sprintf("%d unresolved critical case(s) require attention.", criticalCaseCount),
		})
	default:
		checks = append(checks, TenantHealthCheckResponse{
			Key:    "support",
			Label:  "Critical support queue",
			Status: "pass",
			Detail: "No unresolved critical support case is recorded.",
		})
	}

	return r.SendEnvelope(TenantSupportHealthResponse{
		Status:    productCommercialHealthStatus(checks),
		Checks:    checks,
		CheckedAt: healthNow,
	})
}

func productCommercialValidSupportSeverity(severity string) bool {
	switch severity {
	case "low", "normal", "high", "critical":
		return true
	default:
		return false
	}
}

func productCommercialValidSupportStatus(status models.SupportCaseStatus) bool {
	switch status {
	case models.SupportCaseStatusOpen,
		models.SupportCaseStatusInvestigating,
		models.SupportCaseStatusWaiting,
		models.SupportCaseStatusWaitingCustomer,
		models.SupportCaseStatusWaitingInternal,
		models.SupportCaseStatusResolved,
		models.SupportCaseStatusClosed:
		return true
	default:
		return false
	}
}

func productCommercialSupportTransitionAllowed(
	from, to models.SupportCaseStatus,
) bool {
	if from == to {
		return true
	}
	waiting := map[models.SupportCaseStatus]bool{
		models.SupportCaseStatusWaiting:         true,
		models.SupportCaseStatusWaitingCustomer: true,
		models.SupportCaseStatusWaitingInternal: true,
	}
	if from == models.SupportCaseStatusClosed {
		return false
	}
	if to == models.SupportCaseStatusClosed {
		return true
	}
	if from == models.SupportCaseStatusResolved {
		return to == models.SupportCaseStatusOpen ||
			to == models.SupportCaseStatusInvestigating
	}
	if waiting[from] {
		return to == models.SupportCaseStatusOpen ||
			to == models.SupportCaseStatusInvestigating ||
			to == models.SupportCaseStatusResolved ||
			waiting[to]
	}
	return to == models.SupportCaseStatusOpen ||
		to == models.SupportCaseStatusInvestigating ||
		to == models.SupportCaseStatusResolved ||
		waiting[to]
}

func productCommercialValidateSupportText(title, description string) error {
	if strings.TrimSpace(title) == "" {
		return errors.New("title is required")
	}
	if utf8.RuneCountInString(strings.TrimSpace(title)) > 255 {
		return errors.New("title must be at most 255 characters")
	}
	if strings.TrimSpace(description) == "" {
		return errors.New("description is required")
	}
	if utf8.RuneCountInString(strings.TrimSpace(description)) > 4000 {
		return errors.New("description must be at most 4000 characters")
	}
	return nil
}

// ListSupportCases returns a tenant-scoped support queue.
func (a *App) ListSupportCases(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceSupport, models.ActionRead)
	if err != nil {
		return nil
	}
	pg := parsePagination(r)
	query := a.DB.Model(&models.SupportCase{}).Where("organization_id = ?", orgID)
	if rawStatus := strings.ToLower(strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("status")))); rawStatus != "" {
		status := models.SupportCaseStatus(rawStatus)
		if !productCommercialValidSupportStatus(status) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid support case status", nil, "")
		}
		query = query.Where("status = ?", status)
	}
	if severity := strings.ToLower(strings.TrimSpace(string(r.RequestCtx.QueryArgs().Peek("severity")))); severity != "" {
		if !productCommercialValidSupportSeverity(severity) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid support case severity", nil, "")
		}
		query = query.Where("priority = ?", severity)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return a.sendProductCommercialError(r, "count support cases", err)
	}
	var cases []models.SupportCase
	if err := pg.Apply(query).
		Order("created_at DESC").
		Find(&cases).Error; err != nil {
		return a.sendProductCommercialError(r, "list support cases", err)
	}
	response := make([]SupportCaseResponse, len(cases))
	for i := range cases {
		response[i] = productCommercialSupportCaseToResponse(cases[i])
	}
	return r.SendEnvelope(listEnvelope("cases", response, total, pg))
}

// CreateSupportCase opens a case but never grants support access.
func (a *App) CreateSupportCase(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceSupport, models.ActionWrite)
	if err != nil {
		return nil
	}
	var req CreateSupportCaseRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Description = strings.TrimSpace(req.Description)
	req.Severity = strings.ToLower(strings.TrimSpace(req.Severity))
	req.Category = strings.ToLower(strings.TrimSpace(req.Category))
	if req.Severity == "" {
		req.Severity = "normal"
	}
	if req.Category == "" {
		req.Category = "general"
	}
	if err := productCommercialValidateSupportText(req.Title, req.Description); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	if !productCommercialValidSupportSeverity(req.Severity) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid support case severity", nil, "")
	}
	if !productCommercialValidIdentifier(req.Category, 100) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid support case category", nil, "")
	}

	now := time.Now().UTC()
	caseID := uuid.New()
	supportCase := models.SupportCase{
		BaseModel:      models.BaseModel{ID: caseID},
		OrganizationID: orgID,
		CaseNumber:     fmt.Sprintf("SC-%s-%s", now.Format("20060102"), strings.ToUpper(caseID.String()[:8])),
		Status:         models.SupportCaseStatusOpen,
		Priority:       req.Severity,
		Category:       req.Category,
		Subject:        req.Title,
		Description:    req.Description,
		ReporterUserID: &userID,
		Metadata:       models.JSONB{},
		OpenedAt:       now,
	}
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&supportCase).Error; err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			productSupportCaseAuditResource,
			supportCase.ID,
			models.AuditActionCreated,
			nil,
			productCommercialSupportCaseToResponse(supportCase),
		)
	})
	if err != nil {
		return a.sendProductCommercialError(r, "create support case", err)
	}
	return r.SendEnvelope(productCommercialSupportCaseToResponse(supportCase))
}

// UpdateSupportCase performs an audited tenant-scoped case transition.
func (a *App) UpdateSupportCase(r *fastglue.Request) error {
	orgID, userID, err := a.requireAuth(r, models.ResourceSupport, models.ActionWrite)
	if err != nil {
		return nil
	}
	caseID, err := parsePathUUID(r, "id", "support case")
	if err != nil {
		return nil
	}
	var req UpdateSupportCaseRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if req.Title == nil &&
		req.Description == nil &&
		req.Severity == nil &&
		req.Status == nil &&
		req.AssignedUserID == nil &&
		!req.ClearAssignee &&
		req.Resolution == nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "No support case changes supplied", nil, "")
	}
	if req.Title != nil {
		trimmed := strings.TrimSpace(*req.Title)
		req.Title = &trimmed
	}
	if req.Description != nil {
		trimmed := strings.TrimSpace(*req.Description)
		req.Description = &trimmed
	}
	if req.Title != nil && (*req.Title == "" || utf8.RuneCountInString(*req.Title) > 255) {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"title must be between 1 and 255 characters",
			nil,
			"",
		)
	}
	if req.Description != nil &&
		(*req.Description == "" || utf8.RuneCountInString(*req.Description) > 4000) {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"description must be between 1 and 4000 characters",
			nil,
			"",
		)
	}
	if req.Severity != nil {
		value := strings.ToLower(strings.TrimSpace(*req.Severity))
		req.Severity = &value
		if !productCommercialValidSupportSeverity(value) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid support case severity", nil, "")
		}
	}
	if req.Status != nil && !productCommercialValidSupportStatus(*req.Status) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid support case status", nil, "")
	}
	if req.Resolution != nil {
		trimmed := strings.TrimSpace(*req.Resolution)
		if utf8.RuneCountInString(trimmed) > 4000 {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				"resolution must be at most 4000 characters",
				nil,
				"",
			)
		}
		req.Resolution = &trimmed
	}
	if req.AssignedUserID != nil {
		belongs, err := productCommercialUserBelongsToOrganization(a.DB, orgID, *req.AssignedUserID)
		if err != nil {
			return a.sendProductCommercialError(r, "validate support case assignee", err)
		}
		if !belongs {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				"assigned_user_id does not belong to the organization",
				nil,
				"",
			)
		}
	}

	var updated models.SupportCase
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		var supportCase models.SupportCase
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND organization_id = ?", caseID, orgID).
			First(&supportCase).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return newProductCommercialClientError(fasthttp.StatusNotFound, "Support case not found")
			}
			return err
		}
		oldResponse := productCommercialSupportCaseToResponse(supportCase)
		updates := map[string]any{}
		if req.Title != nil {
			updates["subject"] = *req.Title
			supportCase.Subject = *req.Title
		}
		if req.Description != nil {
			updates["description"] = *req.Description
			supportCase.Description = *req.Description
		}
		if req.Severity != nil {
			updates["priority"] = *req.Severity
			supportCase.Priority = *req.Severity
		}
		if req.Status != nil {
			if !productCommercialSupportTransitionAllowed(supportCase.Status, *req.Status) {
				return newProductCommercialClientError(
					fasthttp.StatusConflict,
					"Invalid support case status transition",
				)
			}
			updates["status"] = *req.Status
			supportCase.Status = *req.Status
			now := time.Now().UTC()
			switch *req.Status {
			case models.SupportCaseStatusResolved:
				updates["resolved_at"] = now
				supportCase.ResolvedAt = &now
			case models.SupportCaseStatusClosed:
				updates["closed_at"] = now
				supportCase.ClosedAt = &now
			case models.SupportCaseStatusOpen, models.SupportCaseStatusInvestigating:
				updates["resolved_at"] = nil
				updates["closed_at"] = nil
				supportCase.ResolvedAt = nil
				supportCase.ClosedAt = nil
			}
		}
		if req.ClearAssignee {
			updates["assigned_to_id"] = nil
			supportCase.AssignedToID = nil
		} else if req.AssignedUserID != nil {
			updates["assigned_to_id"] = *req.AssignedUserID
			supportCase.AssignedToID = req.AssignedUserID
		}
		if req.Resolution != nil {
			updates["resolution"] = *req.Resolution
			supportCase.Resolution = *req.Resolution
		}
		if len(updates) == 0 {
			updated = supportCase
			return nil
		}
		if err := tx.Model(&models.SupportCase{}).
			Where("id = ? AND organization_id = ?", supportCase.ID, orgID).
			Updates(updates).Error; err != nil {
			return err
		}
		if err := audit.LogAudit(
			tx,
			orgID,
			userID,
			audit.GetUserName(tx, userID),
			productSupportCaseAuditResource,
			supportCase.ID,
			models.AuditActionUpdated,
			oldResponse,
			productCommercialSupportCaseToResponse(supportCase),
		); err != nil {
			return err
		}
		updated = supportCase
		return nil
	})
	if err != nil {
		return a.sendProductCommercialError(r, "update support case", err)
	}
	return r.SendEnvelope(productCommercialSupportCaseToResponse(updated))
}

// GetRecoverySummary returns checkpoint metadata with all storage secrets removed.
func (a *App) GetRecoverySummary(r *fastglue.Request) error {
	orgID, _, err := a.requireAuth(r, models.ResourceSupport, models.ActionRead)
	if err != nil {
		return nil
	}
	var checkpoints []models.RecoveryCheckpoint
	if err := a.DB.Where("organization_id = ?", orgID).
		Order("created_at DESC").
		Limit(20).
		Find(&checkpoints).Error; err != nil {
		return a.sendProductCommercialError(r, "load recovery summary", err)
	}
	response := RecoverySummaryResponse{
		Checkpoints: make([]RecoveryCheckpointResponse, len(checkpoints)),
	}
	now := time.Now().UTC()
	for i := range checkpoints {
		response.Checkpoints[i] = productCommercialRecoveryToResponse(checkpoints[i])
		if checkpoints[i].VerifiedAt != nil &&
			(response.LastVerifiedAt == nil || checkpoints[i].VerifiedAt.After(*response.LastVerifiedAt)) {
			verifiedAt := *checkpoints[i].VerifiedAt
			response.LastVerifiedAt = &verifiedAt
		}
		if checkpoints[i].Status == models.RecoveryCheckpointStatusReady &&
			checkpoints[i].VerifiedAt != nil &&
			(checkpoints[i].ExpiresAt == nil || checkpoints[i].ExpiresAt.After(now)) {
			response.RecoveryReady = true
		}
	}
	return r.SendEnvelope(response)
}

func productCommercialValidPlanStatus(status models.CommercialPlanStatus) bool {
	switch status {
	case models.CommercialPlanStatusDraft,
		models.CommercialPlanStatusActive,
		models.CommercialPlanStatusArchived:
		return true
	default:
		return false
	}
}

func productCommercialValidVertical(vertical string) bool {
	switch vertical {
	case "general", "clinic", "pharmacy", "wellness":
		return true
	default:
		return false
	}
}

func productCommercialValidCurrency(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for _, char := range currency {
		if char < 'A' || char > 'Z' {
			return false
		}
	}
	return true
}

func productCommercialValidatePlanPrice(input *PlanPriceInput) error {
	input.Code = strings.ToLower(strings.TrimSpace(input.Code))
	input.Currency = strings.ToUpper(strings.TrimSpace(input.Currency))
	input.TaxBehavior = strings.ToLower(strings.TrimSpace(input.TaxBehavior))
	if !productCommercialValidIdentifier(input.Code, 100) {
		return errors.New("price code must use lowercase letters, numbers, dots, dashes, or underscores")
	}
	if !productCommercialValidCurrency(input.Currency) {
		return errors.New("price currency must be a three-letter ISO code")
	}
	if input.UnitAmountMinor < 0 || input.SetupAmountMinor < 0 {
		return errors.New("price amounts cannot be negative")
	}
	switch input.Interval {
	case models.BillingIntervalOneTime, models.BillingIntervalMonth, models.BillingIntervalYear:
	default:
		return errors.New("price interval must be one_time, month, or year")
	}
	if input.IntervalCount == 0 {
		input.IntervalCount = 1
	}
	if input.IntervalCount < 1 || input.IntervalCount > 36 {
		return errors.New("interval_count must be between 1 and 36")
	}
	if input.TaxBehavior == "" {
		input.TaxBehavior = "exclusive"
	}
	switch input.TaxBehavior {
	case "exclusive", "inclusive", "unspecified":
	default:
		return errors.New("tax_behavior must be exclusive, inclusive, or unspecified")
	}
	return nil
}

func productCommercialIntegerValue(value any) bool {
	switch typed := value.(type) {
	case int:
		return true
	case int32:
		return true
	case int64:
		return true
	case float64:
		return !math.IsNaN(typed) && !math.IsInf(typed, 0) && math.Trunc(typed) == typed
	default:
		return false
	}
}

func productCommercialValidatePlanEntitlement(input *PlanEntitlementInput) error {
	input.Key = strings.ToLower(strings.TrimSpace(input.Key))
	input.OverageCurrency = strings.ToUpper(strings.TrimSpace(input.OverageCurrency))
	input.Description = strings.TrimSpace(input.Description)
	if !productCommercialValidIdentifier(input.Key, 150) {
		return errors.New("entitlement key must use lowercase letters, numbers, dots, dashes, or underscores")
	}
	switch input.ValueType {
	case models.EntitlementValueTypeBoolean:
		if _, ok := input.Value.(bool); !ok {
			return fmt.Errorf("entitlement %s requires a boolean value", input.Key)
		}
	case models.EntitlementValueTypeInteger:
		if !productCommercialIntegerValue(input.Value) {
			return fmt.Errorf("entitlement %s requires an integer value", input.Key)
		}
	case models.EntitlementValueTypeString:
		if _, ok := input.Value.(string); !ok {
			return fmt.Errorf("entitlement %s requires a string value", input.Key)
		}
	case models.EntitlementValueTypeJSON:
	default:
		return fmt.Errorf("entitlement %s has an invalid value_type", input.Key)
	}
	if input.Enforcement == "" {
		input.Enforcement = models.EntitlementEnforcementHard
	}
	switch input.Enforcement {
	case models.EntitlementEnforcementNone,
		models.EntitlementEnforcementSoft,
		models.EntitlementEnforcementHard:
	default:
		return fmt.Errorf("entitlement %s has an invalid enforcement", input.Key)
	}
	if input.ResetInterval != "" {
		switch input.ResetInterval {
		case models.BillingIntervalMonth, models.BillingIntervalYear:
		default:
			return fmt.Errorf("entitlement %s reset_interval must be month or year", input.Key)
		}
	}
	if input.OverageUnitAmountMinor < 0 {
		return fmt.Errorf("entitlement %s overage amount cannot be negative", input.Key)
	}
	if input.OverageUnitAmountMinor > 0 && !productCommercialValidCurrency(input.OverageCurrency) {
		return fmt.Errorf("entitlement %s requires a valid overage currency", input.Key)
	}
	if utf8.RuneCountInString(input.Description) > 2000 {
		return fmt.Errorf("entitlement %s description is too long", input.Key)
	}
	data, err := json.Marshal(input.Value)
	if err != nil {
		return fmt.Errorf("entitlement %s value is invalid", input.Key)
	}
	if len(data) > productCommercialMaxEvidenceBytes {
		return fmt.Errorf("entitlement %s value is too large", input.Key)
	}
	return nil
}

func productCommercialValidatePlanCatalog(
	prices []PlanPriceInput,
	entitlements []PlanEntitlementInput,
	requirePrice bool,
) error {
	if requirePrice && len(prices) == 0 {
		return errors.New("at least one price is required")
	}
	priceCodes := make(map[string]struct{}, len(prices))
	for i := range prices {
		if err := productCommercialValidatePlanPrice(&prices[i]); err != nil {
			return err
		}
		if _, duplicate := priceCodes[prices[i].Code]; duplicate {
			return errors.New("duplicate price code")
		}
		priceCodes[prices[i].Code] = struct{}{}
	}
	entitlementKeys := make(map[string]struct{}, len(entitlements))
	for i := range entitlements {
		if err := productCommercialValidatePlanEntitlement(&entitlements[i]); err != nil {
			return err
		}
		if _, duplicate := entitlementKeys[entitlements[i].Key]; duplicate {
			return errors.New("duplicate entitlement key")
		}
		entitlementKeys[entitlements[i].Key] = struct{}{}
	}
	return nil
}

func productCommercialPlanPriceModel(planID uuid.UUID, input PlanPriceInput) models.PlanPrice {
	return models.PlanPrice{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		PlanID:           planID,
		Code:             input.Code,
		Provider:         models.BillingProviderManual,
		Currency:         input.Currency,
		UnitAmountMinor:  input.UnitAmountMinor,
		SetupAmountMinor: input.SetupAmountMinor,
		Interval:         input.Interval,
		IntervalCount:    input.IntervalCount,
		TaxBehavior:      input.TaxBehavior,
		IsActive:         true,
		ProviderData:     models.JSONB{},
		Metadata:         models.JSONB{},
	}
}

func productCommercialPlanEntitlementModel(
	planID uuid.UUID,
	input PlanEntitlementInput,
) models.PlanEntitlement {
	return models.PlanEntitlement{
		BaseModel:              models.BaseModel{ID: uuid.New()},
		PlanID:                 planID,
		Key:                    input.Key,
		ValueType:              input.ValueType,
		Value:                  models.JSONB{"value": input.Value},
		Enforcement:            input.Enforcement,
		ResetInterval:          input.ResetInterval,
		OverageUnitAmountMinor: input.OverageUnitAmountMinor,
		OverageCurrency:        input.OverageCurrency,
		Description:            input.Description,
	}
}

func productCommercialLoadPlanResponse(
	db *gorm.DB,
	plan *models.Plan,
) (ProductPlanResponse, error) {
	var entitlements []models.PlanEntitlement
	if err := db.Where("plan_id = ?", plan.ID).
		Order("key ASC").
		Find(&entitlements).Error; err != nil {
		return ProductPlanResponse{}, err
	}
	var prices []models.PlanPrice
	if err := db.Where("plan_id = ? AND is_active = ?", plan.ID, true).
		Order("unit_amount_minor ASC").
		Find(&prices).Error; err != nil {
		return ProductPlanResponse{}, err
	}
	entitlementMap := make(map[string]any, len(entitlements))
	for _, entitlement := range entitlements {
		entitlementMap[entitlement.Key] = productCommercialEntitlementValue(entitlement.Value)
	}
	priceResponse := make([]ProductPlanPriceResponse, len(prices))
	for i := range prices {
		priceID := prices[i].ID
		priceResponse[i] = ProductPlanPriceResponse{
			ID:               &priceID,
			Code:             prices[i].Code,
			Currency:         prices[i].Currency,
			UnitAmountMinor:  prices[i].UnitAmountMinor,
			SetupAmountMinor: prices[i].SetupAmountMinor,
			Interval:         prices[i].Interval,
			IntervalCount:    prices[i].IntervalCount,
			TaxBehavior:      prices[i].TaxBehavior,
			Assignable:       productCommercialPlanPriceAssignable(&prices[i]),
		}
	}
	return ProductPlanResponse{
		ID:           &plan.ID,
		Code:         plan.Code,
		Name:         plan.Name,
		Description:  plan.Description,
		Vertical:     plan.Vertical,
		Status:       plan.Status,
		TrialDays:    plan.TrialDays,
		IsPublic:     plan.IsPublic,
		Entitlements: entitlementMap,
		Prices:       priceResponse,
	}, nil
}

func (a *App) productCommercialCanManagePlan(userID uuid.UUID, plan *models.Plan) bool {
	if a.IsSuperAdmin(userID) {
		return true
	}
	return plan.ResellerID != nil && a.canManageReseller(userID, *plan.ResellerID)
}

// productCommercialControlPlaneTransaction keeps cross-tenant catalog writes
// atomic with their tenant-owned audit row. Control-plane routes run outside
// the normal tenant middleware, so production RLS needs an explicit audit
// tenant even though the catalog tables themselves are global.
func (a *App) productCommercialControlPlaneTransaction(
	auditOrgID uuid.UUID,
	fn func(*gorm.DB) error,
) error {
	if a.rlsEnabled() {
		return database.WithTenant(a.rootApp().DB, auditOrgID, fn)
	}
	return a.DB.Transaction(fn)
}

// CreateProductPlan creates a manual-price catalog. Only platform owners may
// create global plans; reseller administrators may create plans for portfolios
// they manage.
func (a *App) CreateProductPlan(r *fastglue.Request) error {
	auditOrgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	var req CreateProductPlanRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	req.Code = strings.ToLower(strings.TrimSpace(req.Code))
	req.Name = strings.TrimSpace(req.Name)
	req.Description = strings.TrimSpace(req.Description)
	req.Vertical = strings.ToLower(strings.TrimSpace(req.Vertical))
	if req.Vertical == "" {
		req.Vertical = "general"
	}
	if req.Status == "" {
		req.Status = models.CommercialPlanStatusDraft
	}
	if !productCommercialValidIdentifier(req.Code, 100) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid plan code", nil, "")
	}
	if req.Name == "" || utf8.RuneCountInString(req.Name) > 255 {
		return r.SendErrorEnvelope(
			fasthttp.StatusBadRequest,
			"Plan name must be between 1 and 255 characters",
			nil,
			"",
		)
	}
	if utf8.RuneCountInString(req.Description) > 4000 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Plan description is too long", nil, "")
	}
	if !productCommercialValidVertical(req.Vertical) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid plan vertical", nil, "")
	}
	if req.Status != models.CommercialPlanStatusDraft &&
		req.Status != models.CommercialPlanStatusActive {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "New plan status must be draft or active", nil, "")
	}
	if req.TrialDays < 0 || req.TrialDays > 365 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "trial_days must be between 0 and 365", nil, "")
	}
	if err := productCommercialValidatePlanCatalog(req.Prices, req.Entitlements, true); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	scopeKey := "platform"
	if req.ResellerID == nil {
		if !a.IsSuperAdmin(userID) {
			return r.SendErrorEnvelope(
				fasthttp.StatusForbidden,
				"Platform owner access required for global plans",
				nil,
				"",
			)
		}
	} else {
		if *req.ResellerID == uuid.Nil || !a.canManageReseller(userID, *req.ResellerID) {
			return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Reseller access denied", nil, "")
		}
		var resellerCount int64
		if err := a.DB.Model(&models.Reseller{}).
			Where("id = ? AND status = ?", *req.ResellerID, models.ResellerStatusActive).
			Count(&resellerCount).Error; err != nil {
			return a.sendProductCommercialError(r, "validate plan reseller", err)
		}
		if resellerCount == 0 {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Active reseller not found", nil, "")
		}
		scopeKey = req.ResellerID.String()
	}

	now := time.Now().UTC()
	plan := models.Plan{
		BaseModel:    models.BaseModel{ID: uuid.New()},
		ResellerID:   req.ResellerID,
		ScopeKey:     scopeKey,
		Code:         req.Code,
		Name:         req.Name,
		Description:  req.Description,
		Status:       req.Status,
		Vertical:     req.Vertical,
		TrialDays:    req.TrialDays,
		DisplayOrder: req.DisplayOrder,
		IsPublic:     req.IsPublic,
		Metadata:     models.JSONB{},
		CreatedByID:  &userID,
	}
	if plan.Status == models.CommercialPlanStatusActive {
		plan.PublishedAt = &now
	}
	var response ProductPlanResponse
	err = a.productCommercialControlPlaneTransaction(auditOrgID, func(tx *gorm.DB) error {
		var duplicateCount int64
		if err := tx.Model(&models.Plan{}).
			Where("scope_key = ? AND code = ?", scopeKey, req.Code).
			Count(&duplicateCount).Error; err != nil {
			return err
		}
		if duplicateCount > 0 {
			return newProductCommercialClientError(
				fasthttp.StatusConflict,
				"A plan with this code already exists in the catalog",
			)
		}
		if err := tx.Create(&plan).Error; err != nil {
			return err
		}
		for _, input := range req.Prices {
			price := productCommercialPlanPriceModel(plan.ID, input)
			if err := tx.Create(&price).Error; err != nil {
				return err
			}
		}
		for _, input := range req.Entitlements {
			entitlement := productCommercialPlanEntitlementModel(plan.ID, input)
			if err := tx.Create(&entitlement).Error; err != nil {
				return err
			}
		}
		response, err = productCommercialLoadPlanResponse(tx, &plan)
		if err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			auditOrgID,
			userID,
			audit.GetUserName(tx, userID),
			productPlanAuditResource,
			plan.ID,
			models.AuditActionCreated,
			nil,
			response,
		)
	})
	if err != nil {
		return a.sendProductCommercialError(r, "create product plan", err)
	}
	return r.SendEnvelope(response)
}

// UpdateProductPlan edits plan metadata and entitlements while keeping
// published prices immutable.
func (a *App) UpdateProductPlan(r *fastglue.Request) error {
	auditOrgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	planID, err := parsePathUUID(r, "id", "product plan")
	if err != nil {
		return nil
	}
	var req UpdateProductPlanRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := productCommercialValidatePlanCatalog(req.NewPrices, req.Entitlements, false); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}
	if req.Name != nil {
		value := strings.TrimSpace(*req.Name)
		if value == "" || utf8.RuneCountInString(value) > 255 {
			return r.SendErrorEnvelope(
				fasthttp.StatusBadRequest,
				"Plan name must be between 1 and 255 characters",
				nil,
				"",
			)
		}
		req.Name = &value
	}
	if req.Description != nil {
		value := strings.TrimSpace(*req.Description)
		if utf8.RuneCountInString(value) > 4000 {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Plan description is too long", nil, "")
		}
		req.Description = &value
	}
	if req.Vertical != nil {
		value := strings.ToLower(strings.TrimSpace(*req.Vertical))
		if !productCommercialValidVertical(value) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid plan vertical", nil, "")
		}
		req.Vertical = &value
	}
	if req.Status != nil && !productCommercialValidPlanStatus(*req.Status) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid plan status", nil, "")
	}
	if req.TrialDays != nil && (*req.TrialDays < 0 || *req.TrialDays > 365) {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "trial_days must be between 0 and 365", nil, "")
	}
	for i := range req.DeactivatePriceCodes {
		req.DeactivatePriceCodes[i] = strings.ToLower(strings.TrimSpace(req.DeactivatePriceCodes[i]))
		if !productCommercialValidIdentifier(req.DeactivatePriceCodes[i], 100) {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid price code to deactivate", nil, "")
		}
	}

	var response ProductPlanResponse
	err = a.productCommercialControlPlaneTransaction(auditOrgID, func(tx *gorm.DB) error {
		var plan models.Plan
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", planID).
			First(&plan).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return newProductCommercialClientError(fasthttp.StatusNotFound, "Product plan not found")
			}
			return err
		}
		if !a.productCommercialCanManagePlan(userID, &plan) {
			return newProductCommercialClientError(fasthttp.StatusForbidden, "Plan management access denied")
		}
		oldResponse, err := productCommercialLoadPlanResponse(tx, &plan)
		if err != nil {
			return err
		}
		updates := map[string]any{}
		if req.Name != nil {
			updates["name"] = *req.Name
			plan.Name = *req.Name
		}
		if req.Description != nil {
			updates["description"] = *req.Description
			plan.Description = *req.Description
		}
		if req.Vertical != nil {
			updates["vertical"] = *req.Vertical
			plan.Vertical = *req.Vertical
		}
		if req.Status != nil {
			updates["status"] = *req.Status
			plan.Status = *req.Status
			now := time.Now().UTC()
			switch *req.Status {
			case models.CommercialPlanStatusActive:
				if plan.PublishedAt == nil {
					updates["published_at"] = now
					plan.PublishedAt = &now
				}
				updates["archived_at"] = nil
				plan.ArchivedAt = nil
			case models.CommercialPlanStatusArchived:
				updates["archived_at"] = now
				plan.ArchivedAt = &now
			}
		}
		if req.TrialDays != nil {
			updates["trial_days"] = *req.TrialDays
			plan.TrialDays = *req.TrialDays
		}
		if req.DisplayOrder != nil {
			updates["display_order"] = *req.DisplayOrder
			plan.DisplayOrder = *req.DisplayOrder
		}
		if req.IsPublic != nil {
			updates["is_public"] = *req.IsPublic
			plan.IsPublic = *req.IsPublic
		}
		if len(updates) > 0 {
			if err := tx.Model(&models.Plan{}).Where("id = ?", plan.ID).Updates(updates).Error; err != nil {
				return err
			}
		}
		for _, input := range req.NewPrices {
			var count int64
			if err := tx.Model(&models.PlanPrice{}).
				Where("plan_id = ? AND code = ?", plan.ID, input.Code).
				Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				return newProductCommercialClientError(
					fasthttp.StatusConflict,
					"Price codes are immutable; use a new code",
				)
			}
			price := productCommercialPlanPriceModel(plan.ID, input)
			if err := tx.Create(&price).Error; err != nil {
				return err
			}
		}
		for _, code := range req.DeactivatePriceCodes {
			result := tx.Model(&models.PlanPrice{}).
				Where("plan_id = ? AND code = ?", plan.ID, code).
				Update("is_active", false)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return newProductCommercialClientError(
					fasthttp.StatusBadRequest,
					"Price code to deactivate was not found",
				)
			}
		}
		for _, input := range req.Entitlements {
			var entitlement models.PlanEntitlement
			findErr := tx.Where("plan_id = ? AND key = ?", plan.ID, input.Key).
				First(&entitlement).Error
			if errors.Is(findErr, gorm.ErrRecordNotFound) {
				entitlement = productCommercialPlanEntitlementModel(plan.ID, input)
				if err := tx.Create(&entitlement).Error; err != nil {
					return err
				}
				continue
			}
			if findErr != nil {
				return findErr
			}
			if err := tx.Model(&models.PlanEntitlement{}).
				Where("id = ? AND plan_id = ?", entitlement.ID, plan.ID).
				Updates(map[string]any{
					"value_type":                input.ValueType,
					"value":                     models.JSONB{"value": input.Value},
					"enforcement":               input.Enforcement,
					"reset_interval":            input.ResetInterval,
					"overage_unit_amount_minor": input.OverageUnitAmountMinor,
					"overage_currency":          input.OverageCurrency,
					"description":               input.Description,
				}).Error; err != nil {
				return err
			}
		}
		response, err = productCommercialLoadPlanResponse(tx, &plan)
		if err != nil {
			return err
		}
		return audit.LogAudit(
			tx,
			auditOrgID,
			userID,
			audit.GetUserName(tx, userID),
			productPlanAuditResource,
			plan.ID,
			models.AuditActionUpdated,
			oldResponse,
			response,
		)
	})
	if err != nil {
		return a.sendProductCommercialError(r, "update product plan", err)
	}
	return r.SendEnvelope(response)
}

func (a *App) productCommercialCanManageOrganization(
	userID uuid.UUID,
	organization *models.Organization,
) bool {
	if a.IsSuperAdmin(userID) {
		return true
	}
	return organization.ResellerID != nil &&
		a.canManageReseller(userID, *organization.ResellerID)
}

func productCommercialSubscriptionPeriodEnd(
	start time.Time,
	price *models.PlanPrice,
) time.Time {
	count := price.IntervalCount
	if count < 1 {
		count = 1
	}
	switch price.Interval {
	case models.BillingIntervalYear:
		return start.AddDate(count, 0, 0)
	default:
		return start.AddDate(0, count, 0)
	}
}

func productCommercialEntitlementSnapshot(
	db *gorm.DB,
	planID uuid.UUID,
) (models.JSONB, error) {
	var entitlements []models.PlanEntitlement
	if err := db.Where("plan_id = ?", planID).Order("key ASC").Find(&entitlements).Error; err != nil {
		return nil, err
	}
	snapshot := models.JSONB{}
	for _, entitlement := range entitlements {
		snapshot[entitlement.Key] = productCommercialEntitlementValue(entitlement.Value)
	}
	return snapshot, nil
}

func productCommercialValidateSetSubscription(req *SetOrganizationSubscriptionRequest) error {
	req.PlanCode = strings.ToLower(strings.TrimSpace(req.PlanCode))
	req.PriceCode = strings.ToLower(strings.TrimSpace(req.PriceCode))
	req.ManualReference = strings.TrimSpace(req.ManualReference)
	if (req.PlanID == nil || *req.PlanID == uuid.Nil) && req.PlanCode == "" {
		return errors.New("plan_id or plan_code is required")
	}
	if req.PlanCode != "" && !productCommercialValidIdentifier(req.PlanCode, 100) {
		return errors.New("invalid plan_code")
	}
	if (req.PlanPriceID == nil || *req.PlanPriceID == uuid.Nil) && req.PriceCode == "" {
		return errors.New("plan_price_id or price_code is required")
	}
	if req.PriceCode != "" && !productCommercialValidIdentifier(req.PriceCode, 100) {
		return errors.New("invalid price_code")
	}
	if req.Status == "" {
		req.Status = models.SubscriptionStatusActive
	}
	if req.Status != models.SubscriptionStatusActive &&
		req.Status != models.SubscriptionStatusTrialing {
		return errors.New("manual subscription status must be active or trialing")
	}
	if req.TrialDays != nil {
		if req.Status != models.SubscriptionStatusTrialing {
			return errors.New("trial_days is only valid for a trialing subscription")
		}
		if *req.TrialDays < 1 || *req.TrialDays > 365 {
			return errors.New("trial_days must be between 1 and 365")
		}
	}
	if req.ManualReference == "" {
		return errors.New("manual_reference is required")
	}
	if strings.IndexFunc(req.ManualReference, unicode.IsControl) >= 0 {
		return errors.New("manual_reference must not contain control characters")
	}
	if utf8.RuneCountInString(req.ManualReference) > 255 {
		return errors.New("manual_reference must be at most 255 characters")
	}
	return nil
}

func productCommercialFindSubscriptionPlan(
	tx *gorm.DB,
	organization *models.Organization,
	req *SetOrganizationSubscriptionRequest,
) (*models.Plan, *models.PlanPrice, error) {
	query := tx.Where("status = ?", models.CommercialPlanStatusActive)
	if req.PlanID != nil && *req.PlanID != uuid.Nil {
		query = query.Where("id = ?", *req.PlanID)
	} else {
		query = query.Where("code = ?", req.PlanCode)
	}
	if organization.ResellerID != nil {
		query = query.Where("reseller_id IS NULL OR reseller_id = ?", *organization.ResellerID).
			Order(clause.Expr{
				SQL:  "CASE WHEN reseller_id = ? THEN 0 ELSE 1 END",
				Vars: []any{*organization.ResellerID},
			})
	} else {
		query = query.Where("reseller_id IS NULL")
	}
	var plan models.Plan
	if err := query.First(&plan).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, newProductCommercialClientError(
				fasthttp.StatusBadRequest,
				"Active plan is not available to this organization",
			)
		}
		return nil, nil, err
	}

	now := time.Now().UTC()
	priceQuery := tx.Where(
		"plan_id = ? AND provider = ? AND is_active = ? AND (effective_from IS NULL OR effective_from <= ?) AND (effective_until IS NULL OR effective_until > ?)",
		plan.ID,
		models.BillingProviderManual,
		true,
		now,
		now,
	)
	if req.PlanPriceID != nil && *req.PlanPriceID != uuid.Nil {
		priceQuery = priceQuery.Where("id = ?", *req.PlanPriceID)
	} else {
		priceQuery = priceQuery.Where("code = ?", req.PriceCode)
	}
	var price models.PlanPrice
	if err := priceQuery.First(&price).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, newProductCommercialClientError(
				fasthttp.StatusBadRequest,
				"Active manual plan price not found",
			)
		}
		return nil, nil, err
	}
	if price.Interval != models.BillingIntervalMonth &&
		price.Interval != models.BillingIntervalYear {
		return nil, nil, newProductCommercialClientError(
			fasthttp.StatusBadRequest,
			"Only monthly or yearly prices can be assigned as subscriptions",
		)
	}
	if !productCommercialPlanPriceAssignable(&price) {
		return nil, nil, newProductCommercialClientError(
			fasthttp.StatusBadRequest,
			"This plan price is retired and cannot be assigned",
		)
	}
	return &plan, &price, nil
}

// SetOrganizationSubscription lets a platform owner provision a manual active
// or trial license. It cannot mark invoices paid or emulate a verified
// billing-provider webhook.
func (a *App) SetOrganizationSubscription(r *fastglue.Request) error {
	auditOrgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.IsSuperAdmin(userID) {
		return r.SendErrorEnvelope(
			fasthttp.StatusForbidden,
			"Platform owner access required",
			nil,
			"",
		)
	}
	targetOrgID, err := targetOrganizationID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid organization ID", nil, "")
	}
	var req SetOrganizationSubscriptionRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}
	if err := productCommercialValidateSetSubscription(&req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, err.Error(), nil, "")
	}

	var response ProductSubscriptionResponse
	err = database.WithTenant(a.DB, targetOrgID, func(tx *gorm.DB) error {
		var organization models.Organization
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", targetOrgID).
			First(&organization).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return newProductCommercialClientError(fasthttp.StatusNotFound, "Organization not found")
			}
			return err
		}
		plan, price, err := productCommercialFindSubscriptionPlan(tx, &organization, &req)
		if err != nil {
			return err
		}
		snapshot, err := productCommercialEntitlementSnapshot(tx, plan.ID)
		if err != nil {
			return err
		}

		var billingAccount models.BillingAccount
		err = tx.Where("organization_id = ?", targetOrgID).First(&billingAccount).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			billingAccount = models.BillingAccount{
				BaseModel:       models.BaseModel{ID: uuid.New()},
				OrganizationID:  targetOrgID,
				ResellerID:      organization.ResellerID,
				Provider:        models.BillingProviderManual,
				Status:          models.BillingAccountStatusActive,
				DefaultCurrency: price.Currency,
				BillingProfile:  models.JSONB{},
				ProviderData:    models.JSONB{},
				Metadata:        models.JSONB{},
			}
			if err := tx.Create(&billingAccount).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if billingAccount.Provider != models.BillingProviderManual {
			return newProductCommercialClientError(
				fasthttp.StatusConflict,
				"Provider-managed billing accounts cannot be changed by manual licensing",
			)
		}

		now := time.Now().UTC()
		periodEnd := productCommercialSubscriptionPeriodEnd(now, price)
		var trialEndsAt *time.Time
		if req.Status == models.SubscriptionStatusTrialing {
			trialDays := plan.TrialDays
			if req.TrialDays != nil {
				trialDays = *req.TrialDays
			}
			if trialDays < 1 {
				return newProductCommercialClientError(
					fasthttp.StatusBadRequest,
					"The plan has no trial duration; provide trial_days",
				)
			}
			end := now.Add(time.Duration(trialDays) * 24 * time.Hour)
			trialEndsAt = &end
			periodEnd = end
		}
		providerData := models.JSONB{
			"manual_reference": req.ManualReference,
		}

		var current models.Subscription
		currentErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where(
				"organization_id = ? AND status IN ?",
				targetOrgID,
				[]models.SubscriptionStatus{
					models.SubscriptionStatusIncomplete,
					models.SubscriptionStatusTrialing,
					models.SubscriptionStatusActive,
					models.SubscriptionStatusPastDue,
					models.SubscriptionStatusPaused,
				},
			).
			Order("created_at DESC").
			First(&current).Error
		var oldResponse any
		var oldPlan models.Plan
		if currentErr == nil {
			if current.Provider != models.BillingProviderManual {
				return newProductCommercialClientError(
					fasthttp.StatusConflict,
					"Provider-managed subscriptions cannot be changed by manual licensing",
				)
			}
			_ = tx.Where("id = ?", current.PlanID).First(&oldPlan).Error
			oldResponse = productCommercialSubscriptionToAuditSnapshotAt(
				&current,
				&oldPlan,
				now,
			)
		} else if !errors.Is(currentErr, gorm.ErrRecordNotFound) {
			return currentErr
		}

		var subscription models.Subscription
		createdSubscription := false
		if currentErr == nil &&
			current.PlanID == plan.ID &&
			current.PlanPriceID != nil &&
			*current.PlanPriceID == price.ID {
			updates := map[string]any{
				"billing_account_id":    billingAccount.ID,
				"provider":              models.BillingProviderManual,
				"status":                req.Status,
				"quantity":              1,
				"collection_method":     "manual",
				"entitlements_snapshot": snapshot,
				"provider_data":         providerData,
				"current_period_start":  now,
				"current_period_end":    periodEnd,
				"trial_ends_at":         trialEndsAt,
				"grace_until":           nil,
				"cancel_at_period_end":  false,
				"cancel_at":             nil,
				"canceled_at":           nil,
				"ended_at":              nil,
			}
			if err := tx.Model(&models.Subscription{}).
				Where("id = ? AND organization_id = ?", current.ID, targetOrgID).
				Updates(updates).Error; err != nil {
				return err
			}
			subscription = current
			subscription.BillingAccountID = billingAccount.ID
			subscription.Provider = models.BillingProviderManual
			subscription.Status = req.Status
			subscription.EntitlementsSnapshot = snapshot
			subscription.ProviderData = providerData
			subscription.CurrentPeriodStart = &now
			subscription.CurrentPeriodEnd = &periodEnd
			subscription.TrialEndsAt = trialEndsAt
			subscription.GraceUntil = nil
			subscription.CancelAtPeriodEnd = false
			subscription.CancelAt = nil
			subscription.CanceledAt = nil
			subscription.EndedAt = nil
		} else {
			if currentErr == nil {
				if err := tx.Model(&models.Subscription{}).
					Where("id = ? AND organization_id = ?", current.ID, targetOrgID).
					Updates(map[string]any{
						"status":      models.SubscriptionStatusCanceled,
						"canceled_at": now,
						"ended_at":    now,
					}).Error; err != nil {
					return err
				}
				canceled := current
				canceled.Status = models.SubscriptionStatusCanceled
				canceled.CanceledAt = &now
				canceled.EndedAt = &now
				if err := audit.LogAudit(
					tx,
					targetOrgID,
					userID,
					audit.GetUserName(tx, userID),
					productSubscriptionAuditResource,
					current.ID,
					models.AuditActionUpdated,
					oldResponse,
					productCommercialSubscriptionToAuditSnapshotAt(
						&canceled,
						&oldPlan,
						now,
					),
				); err != nil {
					return err
				}
			}
			createdSubscription = true
			subscription = models.Subscription{
				BaseModel:            models.BaseModel{ID: uuid.New()},
				OrganizationID:       targetOrgID,
				BillingAccountID:     billingAccount.ID,
				PlanID:               plan.ID,
				PlanPriceID:          &price.ID,
				Provider:             models.BillingProviderManual,
				Status:               req.Status,
				Quantity:             1,
				CollectionMethod:     "manual",
				EntitlementsSnapshot: snapshot,
				ProviderData:         providerData,
				CurrentPeriodStart:   &now,
				CurrentPeriodEnd:     &periodEnd,
				TrialEndsAt:          trialEndsAt,
				CreatedByID:          &userID,
			}
			if err := tx.Create(&subscription).Error; err != nil {
				return err
			}
		}
		response = productCommercialSubscriptionToResponseAt(&subscription, plan, now)
		auditResponse := productCommercialSubscriptionToAuditSnapshotAt(
			&subscription,
			plan,
			now,
		)
		action := models.AuditActionUpdated
		auditOldData := oldResponse
		if createdSubscription {
			action = models.AuditActionCreated
			auditOldData = nil
		}
		return audit.LogAudit(
			tx,
			targetOrgID,
			userID,
			audit.GetUserName(tx, userID),
			productSubscriptionAuditResource,
			subscription.ID,
			action,
			auditOldData,
			auditResponse,
			map[string]any{
				"field":     "licensed_by_workspace",
				"old_value": nil,
				"new_value": auditOrgID,
			},
		)
	})
	if err != nil {
		return a.sendProductCommercialError(r, "set organization subscription", err)
	}
	return r.SendEnvelope(response)
}

// GetOrganizationSubscription returns a target tenant's safe license summary
// to platform owners and authorized reseller administrators.
func (a *App) GetOrganizationSubscription(r *fastglue.Request) error {
	_, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	targetOrgID, err := targetOrganizationID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid organization ID", nil, "")
	}
	var organization models.Organization
	if err := a.DB.Select("id", "reseller_id").
		Where("id = ?", targetOrgID).
		First(&organization).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Organization not found", nil, "")
		}
		return a.sendProductCommercialError(r, "load organization subscription", err)
	}
	if !a.productCommercialCanManageOrganization(userID, &organization) {
		return r.SendErrorEnvelope(
			fasthttp.StatusForbidden,
			"Organization licensing access denied",
			nil,
			"",
		)
	}
	var response ProductSubscriptionResponse
	err = database.WithTenant(a.DB, targetOrgID, func(tx *gorm.DB) error {
		var subscription models.Subscription
		err := productCommercialLoadCurrentSubscription(tx, targetOrgID, &subscription)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			response = productCommercialSubscriptionToResponse(nil, nil)
			return nil
		}
		if err != nil {
			return err
		}
		var plan models.Plan
		if err := tx.Where("id = ?", subscription.PlanID).First(&plan).Error; err != nil &&
			!errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		response = productCommercialSubscriptionToResponse(&subscription, &plan)
		return nil
	})
	if err != nil {
		return a.sendProductCommercialError(r, "load organization subscription", err)
	}
	return r.SendEnvelope(response)
}
