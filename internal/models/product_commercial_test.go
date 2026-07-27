package models_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProductCommercialTableNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"plan", (models.Plan{}).TableName(), "plans"},
		{"plan price", (models.PlanPrice{}).TableName(), "plan_prices"},
		{"plan entitlement", (models.PlanEntitlement{}).TableName(), "plan_entitlements"},
		{"billing account", (models.BillingAccount{}).TableName(), "billing_accounts"},
		{"subscription", (models.Subscription{}).TableName(), "subscriptions"},
		{"invoice", (models.Invoice{}).TableName(), "invoices"},
		{"billing webhook event", (models.BillingWebhookEvent{}).TableName(), "billing_webhook_events"},
		{"entitlement override", (models.EntitlementOverride{}).TableName(), "entitlement_overrides"},
		{"usage event", (models.UsageEvent{}).TableName(), "usage_events"},
		{"billing usage rollup", (models.BillingUsageRollup{}).TableName(), "billing_usage_rollups"},
		{"organization onboarding", (models.OrganizationOnboarding{}).TableName(), "organization_onboardings"},
		{"provisioning run", (models.ProvisioningRun{}).TableName(), "provisioning_runs"},
		{"workspace template", (models.WorkspaceTemplate{}).TableName(), "workspace_templates"},
		{"workspace template version", (models.WorkspaceTemplateVersion{}).TableName(), "workspace_template_versions"},
		{"workspace template application", (models.WorkspaceTemplateApplication{}).TableName(), "workspace_template_applications"},
		{"workspace template resource map", (models.WorkspaceTemplateResourceMap{}).TableName(), "workspace_template_resource_maps"},
		{"consent event", (models.ConsentEvent{}).TableName(), "consent_events"},
		{"consent state", (models.ConsentState{}).TableName(), "consent_states"},
		{"retention policy", (models.RetentionPolicy{}).TableName(), "retention_policies"},
		{"privacy request", (models.PrivacyRequest{}).TableName(), "privacy_requests"},
		{"privacy request event", (models.PrivacyRequestEvent{}).TableName(), "privacy_request_events"},
		{"privacy job", (models.PrivacyJob{}).TableName(), "privacy_jobs"},
		{"legal hold", (models.LegalHold{}).TableName(), "legal_holds"},
		{"breach incident", (models.BreachIncident{}).TableName(), "breach_incidents"},
		{"support access grant", (models.SupportAccessGrant{}).TableName(), "support_access_grants"},
		{"support case", (models.SupportCase{}).TableName(), "support_cases"},
		{"recovery checkpoint", (models.RecoveryCheckpoint{}).TableName(), "recovery_checkpoints"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.got)
		})
	}
}

func TestProductCommercialConstants(t *testing.T) {
	t.Parallel()

	assert.Equal(t, models.SubscriptionStatus("active"), models.SubscriptionStatusActive)
	assert.Equal(t, models.InvoiceStatus("paid"), models.InvoiceStatusPaid)
	assert.Equal(t, models.OnboardingStatus("provisioning"), models.OnboardingStatusProvisioning)
	assert.Equal(t, models.WorkspaceTemplateStatus("published"), models.WorkspaceTemplateStatusPublished)
	assert.Equal(t, models.PrivacyRequestStatus("awaiting_verification"), models.PrivacyRequestStatusAwaitingVerification)
	assert.Equal(t, models.SupportGrantStatus("active"), models.SupportGrantStatusActive)
	assert.Equal(t, models.RecoveryCheckpointStatus("ready"), models.RecoveryCheckpointStatusReady)
}

func TestProductCommercialSensitiveFieldsAreRedacted(t *testing.T) {
	t.Parallel()

	fixtures := []struct {
		name  string
		value any
	}{
		{
			name: "billing webhook",
			value: models.BillingWebhookEvent{
				Payload:   models.JSONB{"secret_payload_marker": "value"},
				Headers:   models.JSONB{"secret_header_marker": "value"},
				Signature: "secret_signature_marker",
			},
		},
		{
			name: "privacy request",
			value: models.PrivacyRequest{
				RequesterProfile:      models.JSONB{"secret_requester_marker": "value"},
				RequestDetails:        models.JSONB{"secret_request_marker": "value"},
				VerificationTokenHash: "secret_verification_marker",
				ResultObjectKey:       "secret_result_marker",
			},
		},
		{
			name: "support grant",
			value: models.SupportAccessGrant{
				AccessTokenHash: "secret_access_marker",
				IPAllowlist:     models.StringArray{"secret_ip_marker"},
			},
		},
		{
			name: "recovery checkpoint",
			value: models.RecoveryCheckpoint{
				Scope:            models.JSONB{"secret_scope_marker": "value"},
				StorageProvider:  "secret_provider_marker",
				StorageBucket:    "secret_bucket_marker",
				StorageObjectKey: "secret_object_marker",
				EncryptionKeyRef: "secret_key_marker",
			},
		},
	}

	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(fixture.value)
			require.NoError(t, err)

			serialized := string(data)
			assert.NotContains(t, serialized, "secret_")
			assert.False(t, strings.Contains(serialized, "payload_marker"))
		})
	}
}
