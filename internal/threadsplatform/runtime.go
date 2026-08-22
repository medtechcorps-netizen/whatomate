package threadsplatform

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
)

const (
	RequiredPermissionResource = models.ResourceSettingsIntegrations
	RequiredPermissionAction   = models.ActionWrite
	RequiredEntitlementKey     = "threads.public_engagement.enabled"
)

var (
	ErrManagedDisabled        = errors.New("managed Threads is disabled")
	ErrOrganizationNotAllowed = errors.New("organization is not released for managed Threads")
	ErrComplianceOrganization = errors.New("the platform compliance organization cannot onboard a managed Threads profile")
	ErrUnknownPlatformApp     = errors.New("managed Threads platform app is not configured")
	ErrPermissionRequired     = errors.New("settings.integrations write permission is required")
	ErrEntitlementRequired    = errors.New("threads public engagement entitlement is required")
	ErrInvalidManagementMode  = errors.New("invalid Threads integration management mode")
)

var platformAppKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// AppDescriptor is safe runtime metadata. It intentionally cannot expose the
// app secret, webhook verify token, or review-evidence digest.
type AppDescriptor struct {
	PlatformAppKey          string     `json:"platform_app_key"`
	AppID                   string     `json:"app_id"`
	AppReviewStatus         string     `json:"app_review_status"`
	AppReviewApprovedAt     *time.Time `json:"app_review_approved_at,omitempty"`
	ConfigurationGeneration uint64     `json:"configuration_generation"`
}

// CredentialMaterial is available only through an explicit runtime method.
// JSON and common formatting paths remain redacted.
type CredentialMaterial struct {
	AppSecret          string `json:"-"`
	WebhookVerifyToken string `json:"-"`
}

func (CredentialMaterial) String() string   { return "[REDACTED]" }
func (CredentialMaterial) GoString() string { return "[REDACTED]" }

type runtimeApp struct {
	descriptor                 AppDescriptor
	credentials                CredentialMaterial
	expectedDevelopmentProfile string
}

// Runtime is the fail-closed deployment policy shared by future OAuth,
// webhook, lifecycle, and administrative entry points.
type Runtime struct {
	enabled                  bool
	allowAllOrganizations    bool
	allowedOrganizations     map[uuid.UUID]struct{}
	complianceOrganizationID uuid.UUID
	reReplyBaseURL           string
	apps                     map[string]runtimeApp
}

// ValidatedRuntime is the only object that can release deployment-owned
// credential material. It can be obtained only after the configured
// compliance organization has been checked against durable database state.
// This makes the prerequisite difficult for later OAuth/webhook entry points
// to omit accidentally.
type ValidatedRuntime struct {
	runtime *Runtime
}

// NewRuntime revalidates deployment config rather than assuming the caller
// obtained it through config.Load. This protects independently constructed
// worker and test configurations.
func NewRuntime(
	managed config.ThreadsManagedConfig,
	development config.ThreadsAppReviewConfig,
	environment string,
) (*Runtime, error) {
	canonicalManaged, err := config.CanonicalizeThreadsManagedConfig(managed)
	if err != nil {
		return nil, err
	}
	managed = canonicalManaged
	if err := config.ValidateThreadsManagedConfig(managed, development, environment); err != nil {
		return nil, err
	}
	runtime := &Runtime{
		enabled:               managed.Enabled,
		allowAllOrganizations: managed.AllowAllOrganizations,
		allowedOrganizations:  make(map[uuid.UUID]struct{}),
		reReplyBaseURL:        managed.ReReplyBaseURL,
		apps:                  make(map[string]runtimeApp),
	}
	if !managed.Enabled {
		return runtime, nil
	}

	complianceOrganizationID, err := uuid.Parse(managed.ComplianceOrganizationID)
	if err != nil || complianceOrganizationID == uuid.Nil {
		if err == nil {
			err = errors.New("uuid is nil")
		}
		return nil, fmt.Errorf("parse managed Threads compliance organization: %w", err)
	}
	runtime.complianceOrganizationID = complianceOrganizationID
	for _, rawOrganizationID := range strings.Split(managed.AllowedOrganizationIDs, ",") {
		rawOrganizationID = strings.TrimSpace(rawOrganizationID)
		if rawOrganizationID == "" {
			continue
		}
		organizationID, parseErr := uuid.Parse(rawOrganizationID)
		if parseErr != nil {
			return nil, fmt.Errorf("parse managed Threads allowed organization: %w", parseErr)
		}
		runtime.allowedOrganizations[organizationID] = struct{}{}
	}
	for _, configuredApp := range managed.PlatformApps {
		var approvedAt *time.Time
		if configuredApp.AppReviewApprovedAt != "" {
			value, _ := time.Parse(time.RFC3339Nano, configuredApp.AppReviewApprovedAt)
			approvedAt = &value
		}
		runtime.apps[configuredApp.PlatformAppKey] = runtimeApp{
			descriptor: AppDescriptor{
				PlatformAppKey:          configuredApp.PlatformAppKey,
				AppID:                   configuredApp.AppID,
				AppReviewStatus:         configuredApp.AppReviewStatus,
				AppReviewApprovedAt:     approvedAt,
				ConfigurationGeneration: configuredApp.ConfigurationGeneration,
			},
			credentials: CredentialMaterial{
				AppSecret:          configuredApp.AppSecret,
				WebhookVerifyToken: configuredApp.WebhookVerifyToken,
			},
			expectedDevelopmentProfile: func() string {
				if configuredApp.AppReviewStatus == "approved" {
					return ""
				}
				return strings.TrimSpace(development.DevelopmentProfileID)
			}(),
		}
	}
	return runtime, nil
}

func (r *Runtime) OrganizationAllowed(organizationID uuid.UUID) bool {
	if r == nil || !r.enabled || organizationID == uuid.Nil ||
		organizationID == r.complianceOrganizationID {
		return false
	}
	if r.allowAllOrganizations {
		return true
	}
	_, allowed := r.allowedOrganizations[organizationID]
	return allowed
}

func (r *Runtime) App(platformAppKey string) (AppDescriptor, error) {
	if r == nil || !r.enabled {
		return AppDescriptor{}, ErrManagedDisabled
	}
	app, ok := r.apps[platformAppKey]
	if !ok {
		return AppDescriptor{}, ErrUnknownPlatformApp
	}
	return app.descriptor, nil
}

func (r *ValidatedRuntime) Credentials(platformAppKey string) (CredentialMaterial, error) {
	if r == nil || r.runtime == nil || !r.runtime.enabled {
		return CredentialMaterial{}, ErrManagedDisabled
	}
	app, ok := r.runtime.apps[platformAppKey]
	if !ok {
		return CredentialMaterial{}, ErrUnknownPlatformApp
	}
	return app.credentials, nil
}

// OrganizationAllowed exposes only the already validated deployment release
// decision. Tenant rows cannot expand this allowlist.
func (r *ValidatedRuntime) OrganizationAllowed(organizationID uuid.UUID) bool {
	return r != nil && r.runtime != nil && r.runtime.OrganizationAllowed(organizationID)
}

// App returns safe, non-secret app metadata from the durable-validated
// runtime. It never releases credential material.
func (r *ValidatedRuntime) App(platformAppKey string) (AppDescriptor, error) {
	if r == nil || r.runtime == nil {
		return AppDescriptor{}, ErrManagedDisabled
	}
	return r.runtime.App(platformAppKey)
}

// SoleApp is the fail-closed initial shard selector. Existing managed rows
// retain their immutable key; a deployment with multiple apps must add an
// explicit server-owned shard policy before onboarding a new tenant.
func (r *ValidatedRuntime) SoleApp() (AppDescriptor, error) {
	if r == nil || r.runtime == nil || !r.runtime.enabled {
		return AppDescriptor{}, ErrManagedDisabled
	}
	if len(r.runtime.apps) != 1 {
		return AppDescriptor{}, ErrUnknownPlatformApp
	}
	for _, app := range r.runtime.apps {
		return app.descriptor, nil
	}
	return AppDescriptor{}, ErrUnknownPlatformApp
}

// CallbackURL derives the one global managed callback from deployment-owned
// configuration. No tenant or request value participates in this URL.
func (r *ValidatedRuntime) CallbackURL() (string, error) {
	if r == nil || r.runtime == nil || !r.runtime.enabled || r.runtime.reReplyBaseURL == "" {
		return "", ErrManagedDisabled
	}
	return r.runtime.reReplyBaseURL + "/api/integrations/threads/managed/callback", nil
}

// ValidateActivation repeats the complete release policy through the
// durable-validated capability.
func (r *ValidatedRuntime) ValidateActivation(facts ActivationFacts) error {
	if r == nil || r.runtime == nil {
		return ErrManagedDisabled
	}
	return r.runtime.ValidateActivation(facts)
}

// ValidateProviderIdentity keeps the OAuth subject and Threads authority
// profile distinct. Meta documents the former as the debug-token app-scoped
// user_id and the latter as /me.id; it does not promise they are equal.
func (r *ValidatedRuntime) ValidateProviderIdentity(
	platformAppKey, oauthSubjectID, authorityProfileID string,
) error {
	if r == nil || r.runtime == nil || !r.runtime.enabled ||
		!canonicalNumericID(oauthSubjectID) || !canonicalNumericID(authorityProfileID) {
		return ErrInvalidManagementMode
	}
	app, ok := r.runtime.apps[platformAppKey]
	if !ok {
		return ErrUnknownPlatformApp
	}
	if app.expectedDevelopmentProfile != "" &&
		authorityProfileID != app.expectedDevelopmentProfile {
		return ErrOrganizationNotAllowed
	}
	return nil
}

// ActivationFacts are authorization results supplied by the caller. Runtime
// policy requires both the ordinary integration-write permission and the
// product entitlement; either unknown/false result fails closed.
type ActivationFacts struct {
	OrganizationID                uuid.UUID
	PlatformAppKey                string
	HasIntegrationWritePermission bool
	HasThreadsEntitlement         bool
}

func (r *Runtime) ValidateActivation(facts ActivationFacts) error {
	if r == nil || !r.enabled {
		return ErrManagedDisabled
	}
	if facts.OrganizationID == r.complianceOrganizationID {
		return ErrComplianceOrganization
	}
	if !facts.HasIntegrationWritePermission {
		return ErrPermissionRequired
	}
	if !facts.HasThreadsEntitlement {
		return ErrEntitlementRequired
	}
	if !r.OrganizationAllowed(facts.OrganizationID) {
		return ErrOrganizationNotAllowed
	}
	if _, ok := r.apps[facts.PlatformAppKey]; !ok {
		return ErrUnknownPlatformApp
	}
	return nil
}

// EffectiveManagementMode gives rolling deployments the old behavior when a
// row was read before the additive backfill or constructed by an older binary.
func EffectiveManagementMode(integration *models.ProviderIntegration) string {
	if integration == nil || strings.TrimSpace(integration.ManagementMode) == "" {
		return models.ThreadsManagementModeWorkspaceBYO
	}
	return strings.TrimSpace(integration.ManagementMode)
}

// ValidateIntegrationManagement validates the persistence boundary used by
// future control-plane handlers. It does not mutate or transfer a binding.
func ValidateIntegrationManagement(integration *models.ProviderIntegration) error {
	if integration == nil {
		return ErrInvalidManagementMode
	}
	mode := EffectiveManagementMode(integration)
	if integration.ManagementMode != "" && integration.ManagementMode != mode {
		return ErrInvalidManagementMode
	}
	if integration.Provider != "threads" {
		if mode != models.ThreadsManagementModeWorkspaceBYO || integration.PlatformAppKey != nil {
			return ErrInvalidManagementMode
		}
		return nil
	}

	switch mode {
	case models.ThreadsManagementModeWorkspaceBYO:
		if integration.PlatformAppKey != nil {
			return ErrInvalidManagementMode
		}
		return nil
	case models.ThreadsManagementModePlatformManaged:
		if integration.PlatformAppKey == nil ||
			strings.TrimSpace(*integration.PlatformAppKey) != *integration.PlatformAppKey ||
			!platformAppKeyPattern.MatchString(*integration.PlatformAppKey) ||
			integration.ThreadsAppID != nil ||
			hasAnyKey(integration.CredentialData, "app_secret", "webhook_verify_token") ||
			hasAnyKey(integration.Config, "app_id", "redirect_uri", "app_review_status", "_app_review_approval") {
			return ErrInvalidManagementMode
		}
		return nil
	default:
		return ErrInvalidManagementMode
	}
}

func hasAnyKey(values models.JSONB, keys ...string) bool {
	for _, key := range keys {
		if _, exists := values[key]; exists {
			return true
		}
	}
	return false
}
