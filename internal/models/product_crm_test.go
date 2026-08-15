package models_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tableNamer interface {
	TableName() string
}

func TestProductCRMTableNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		model tableNamer
		want  string
	}{
		{"scheduled job", models.ScheduledJob{}, "scheduled_jobs"},
		{"outbox event", models.OutboxEvent{}, "outbox_events"},
		{"customer activity event", models.CustomerActivityEvent{}, "customer_activity_events"},
		{"CRM pipeline", models.CRMPipeline{}, "crm_pipelines"},
		{"CRM pipeline stage", models.CRMPipelineStage{}, "crm_pipeline_stages"},
		{"CRM lead", models.CRMLead{}, "crm_leads"},
		{"CRM stage history", models.CRMStageHistory{}, "crm_stage_history"},
		{"follow-up task", models.FollowUpTask{}, "follow_up_tasks"},
		{"booking service", models.BookingService{}, "booking_services"},
		{"booking resource", models.BookingResource{}, "booking_resources"},
		{"booking service resource", models.BookingServiceResource{}, "booking_service_resources"},
		{"availability rule", models.AvailabilityRule{}, "availability_rules"},
		{"resource time off", models.ResourceTimeOff{}, "resource_time_off"},
		{"booking event", models.BookingEvent{}, "booking_events"},
		{"booking", models.Booking{}, "bookings"},
		{"package definition", models.PackageDefinition{}, "package_definitions"},
		{"package entitlement", models.PackageEntitlement{}, "package_entitlements"},
		{"contact package", models.ContactPackage{}, "contact_packages"},
		{"credit balance", models.CreditBalance{}, "credit_balances"},
		{"credit ledger entry", models.CreditLedgerEntry{}, "credit_ledger_entries"},
		{"commerce invoice", models.CommerceInvoice{}, "commerce_invoices"},
		{"invoice line", models.InvoiceLine{}, "invoice_lines"},
		{"payment provider account", models.PaymentProviderAccount{}, "payment_provider_accounts"},
		{"payment intent", models.PaymentIntent{}, "payment_intents"},
		{"payment transaction", models.PaymentTransaction{}, "payment_transactions"},
		{"payment webhook event", models.PaymentWebhookEvent{}, "payment_webhook_events"},
		{"Copilot settings", models.CopilotSettings{}, "copilot_settings"},
		{"Copilot run", models.CopilotRun{}, "copilot_runs"},
		{"Copilot feedback", models.CopilotFeedback{}, "copilot_feedback"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.model.TableName())
		})
	}
}

func TestProductCRMSecretsAreRedacted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		value         any
		visibleMarker string
		secretMarkers []string
		forbiddenKeys []string
	}{
		{
			name: "payment provider credentials",
			value: models.PaymentProviderAccount{
				Name:                   "visible-provider-account",
				APIKeyEncrypted:        "secret-api-key-marker",
				APISecretEncrypted:     "secret-api-secret-marker",
				WebhookSecretEncrypted: "secret-webhook-marker",
			},
			visibleMarker: "visible-provider-account",
			secretMarkers: []string{
				"secret-api-key-marker",
				"secret-api-secret-marker",
				"secret-webhook-marker",
			},
			forbiddenKeys: []string{
				"api_key_encrypted",
				"api_secret_encrypted",
				"webhook_secret_encrypted",
				"APIKeyEncrypted",
				"APISecretEncrypted",
				"WebhookSecretEncrypted",
			},
		},
		{
			name: "Copilot API key",
			value: models.CopilotSettings{
				Model:           "visible-copilot-model",
				APIKeyEncrypted: "secret-copilot-key-marker",
			},
			visibleMarker: "visible-copilot-model",
			secretMarkers: []string{"secret-copilot-key-marker"},
			forbiddenKeys: []string{
				"api_key_encrypted",
				"APIKeyEncrypted",
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(tt.value)
			require.NoError(t, err)
			assert.Contains(t, string(data), tt.visibleMarker)
			for _, marker := range tt.secretMarkers {
				assert.NotContains(t, string(data), marker)
			}

			var payload map[string]any
			require.NoError(t, json.Unmarshal(data, &payload))
			for _, key := range tt.forbiddenKeys {
				assert.NotContains(t, payload, key)
			}
		})
	}
}

func TestProductCRMConstants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"scheduled job pending", string(models.ScheduledJobStatusPending), "pending"},
		{"scheduled job generating", string(models.ScheduledJobStatusGenerating), "generating"},
		{"scheduled job cancelled", string(models.ScheduledJobStatusCancelled), "cancelled"},
		{"outbox published", string(models.OutboxEventStatusPublished), "published"},
		{"outbox failed", string(models.OutboxEventStatusFailed), "failed"},
		{"customer activity merge", string(models.CustomerActivityContactMerged), "contact.merged"},
		{"customer activity stage move", string(models.CustomerActivityCRMStageMoved), "crm.lead.stage_moved"},
		{"customer activity invoice paid", string(models.CustomerActivityInvoicePaid), "invoice.paid"},
		{"pipeline stage won", string(models.CRMPipelineStageKindWon), "won"},
		{"lead archived", string(models.CRMLeadStatusArchived), "archived"},
		{"lead WhatsApp source", string(models.CRMLeadSourceWhatsApp), "whatsapp"},
		{"lead walk-in source", string(models.CRMLeadSourceWalkIn), "walk_in"},
		{"follow-up in progress", string(models.FollowUpTaskStatusInProgress), "in_progress"},
		{"follow-up urgent", string(models.FollowUpTaskPriorityUrgent), "urgent"},
		{"booking service class", string(models.BookingServiceKindClass), "class"},
		{"booking resource practitioner", string(models.BookingResourceKindPractitioner), "practitioner"},
		{"booking event cancelled", string(models.BookingEventStatusCancelled), "cancelled"},
		{"booking checked in", string(models.BookingStatusCheckedIn), "checked_in"},
		{"booking no-show", string(models.BookingStatusNoShow), "no_show"},
		{"booking API source", string(models.BookingSourceAPI), "api"},
		{"contact package exhausted", string(models.ContactPackageStatusExhausted), "exhausted"},
		{"credit reserve", string(models.CreditLedgerEntryTypeReserve), "reserve"},
		{"credit reverse", string(models.CreditLedgerEntryTypeReverse), "reverse"},
		{"invoice partially refunded", string(models.CommerceInvoiceStatusPartiallyRefunded), "partially_refunded"},
		{"payment live environment", string(models.PaymentEnvironmentLive), "live"},
		{"payment requires action", string(models.PaymentIntentStatusRequiresAction), "requires_action"},
		{"payment intent expired", string(models.PaymentIntentStatusExpired), "expired"},
		{"payment adjustment", string(models.PaymentTransactionTypeAdjustment), "adjustment"},
		{"payment transaction reversed", string(models.PaymentTransactionStatusReversed), "reversed"},
		{"payment webhook ignored", string(models.PaymentWebhookEventStatusIgnored), "ignored"},
		{"Copilot extract actions", string(models.CopilotTaskTypeExtractActions), "extract_actions"},
		{"Copilot blocked", string(models.CopilotRunStatusBlocked), "blocked"},
		{"Copilot not helpful", string(models.CopilotFeedbackRatingNotHelpful), "not_helpful"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.got)
		})
	}
}

func TestProductCRMOrganizationScopeIsExplicit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		model any
	}{
		{"scheduled job", models.ScheduledJob{}},
		{"outbox event", models.OutboxEvent{}},
		{"customer activity event", models.CustomerActivityEvent{}},
		{"CRM pipeline", models.CRMPipeline{}},
		{"CRM pipeline stage", models.CRMPipelineStage{}},
		{"CRM lead", models.CRMLead{}},
		{"CRM stage history", models.CRMStageHistory{}},
		{"follow-up task", models.FollowUpTask{}},
		{"booking service", models.BookingService{}},
		{"booking resource", models.BookingResource{}},
		{"booking service resource", models.BookingServiceResource{}},
		{"availability rule", models.AvailabilityRule{}},
		{"resource time off", models.ResourceTimeOff{}},
		{"booking event", models.BookingEvent{}},
		{"booking", models.Booking{}},
		{"package definition", models.PackageDefinition{}},
		{"package entitlement", models.PackageEntitlement{}},
		{"contact package", models.ContactPackage{}},
		{"credit balance", models.CreditBalance{}},
		{"credit ledger entry", models.CreditLedgerEntry{}},
		{"commerce invoice", models.CommerceInvoice{}},
		{"invoice line", models.InvoiceLine{}},
		{"payment provider account", models.PaymentProviderAccount{}},
		{"payment intent", models.PaymentIntent{}},
		{"payment transaction", models.PaymentTransaction{}},
		{"payment webhook event", models.PaymentWebhookEvent{}},
		{"Copilot settings", models.CopilotSettings{}},
		{"Copilot run", models.CopilotRun{}},
		{"Copilot feedback", models.CopilotFeedback{}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			modelType := reflect.TypeOf(tt.model)
			field, ok := modelType.FieldByName("OrganizationID")
			require.True(t, ok)
			assert.Len(t, field.Index, 1, "OrganizationID must be declared directly on the table model")
			assert.Equal(t, "uuid.UUID", field.Type.String())
			assert.Equal(t, "organization_id", field.Tag.Get("json"))
			assert.Contains(t, field.Tag.Get("gorm"), "not null")
		})
	}
}

func TestProductCRMMutableModelsHaveOptimisticVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		model any
	}{
		{"scheduled job", models.ScheduledJob{}},
		{"outbox event", models.OutboxEvent{}},
		{"CRM pipeline", models.CRMPipeline{}},
		{"CRM pipeline stage", models.CRMPipelineStage{}},
		{"CRM lead", models.CRMLead{}},
		{"follow-up task", models.FollowUpTask{}},
		{"booking service", models.BookingService{}},
		{"booking resource", models.BookingResource{}},
		{"booking service resource", models.BookingServiceResource{}},
		{"availability rule", models.AvailabilityRule{}},
		{"resource time off", models.ResourceTimeOff{}},
		{"booking event", models.BookingEvent{}},
		{"booking", models.Booking{}},
		{"package definition", models.PackageDefinition{}},
		{"package entitlement", models.PackageEntitlement{}},
		{"contact package", models.ContactPackage{}},
		{"credit balance", models.CreditBalance{}},
		{"commerce invoice", models.CommerceInvoice{}},
		{"invoice line", models.InvoiceLine{}},
		{"payment provider account", models.PaymentProviderAccount{}},
		{"payment intent", models.PaymentIntent{}},
		{"payment webhook event", models.PaymentWebhookEvent{}},
		{"Copilot settings", models.CopilotSettings{}},
		{"Copilot run", models.CopilotRun{}},
		{"Copilot feedback", models.CopilotFeedback{}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			field, ok := reflect.TypeOf(tt.model).FieldByName("Version")
			require.True(t, ok)
			assert.Equal(t, reflect.Int64, field.Type.Kind())
			assert.Equal(t, "version", field.Tag.Get("json"))
			assert.Contains(t, field.Tag.Get("gorm"), "default:1")
		})
	}
}

func TestProductCRMImmutableLedgersDoNotEmbedBaseModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		model any
	}{
		{"credit ledger entry", models.CreditLedgerEntry{}},
		{"payment transaction", models.PaymentTransaction{}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			modelType := reflect.TypeOf(tt.model)
			_, hasBaseModel := modelType.FieldByName("BaseModel")
			assert.False(t, hasBaseModel)

			for _, forbiddenField := range []string{"UpdatedAt", "DeletedAt", "Version"} {
				_, exists := modelType.FieldByName(forbiddenField)
				assert.False(t, exists, "%s must be append-only", forbiddenField)
			}

			_, hasID := modelType.FieldByName("ID")
			_, hasCreatedAt := modelType.FieldByName("CreatedAt")
			assert.True(t, hasID)
			assert.True(t, hasCreatedAt)
		})
	}
}

func TestProductCRMMoneyUsesIntegerMinorUnits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		model  any
		fields []string
	}{
		{"CRM lead", models.CRMLead{}, []string{"ValueMinor"}},
		{"booking service", models.BookingService{}, []string{"PriceMinor"}},
		{"package definition", models.PackageDefinition{}, []string{"PriceMinor"}},
		{"contact package", models.ContactPackage{}, []string{"PurchaseAmountMinor"}},
		{
			"commerce invoice",
			models.CommerceInvoice{},
			[]string{"SubtotalMinor", "DiscountMinor", "TaxMinor", "TotalMinor", "PaidMinor", "DueMinor"},
		},
		{
			"invoice line",
			models.InvoiceLine{},
			[]string{"UnitAmountMinor", "SubtotalMinor", "TaxMinor", "TotalMinor"},
		},
		{"payment intent", models.PaymentIntent{}, []string{"AmountMinor"}},
		{"payment transaction", models.PaymentTransaction{}, []string{"AmountMinor"}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			modelType := reflect.TypeOf(tt.model)
			for _, fieldName := range tt.fields {
				field, ok := modelType.FieldByName(fieldName)
				require.True(t, ok)
				assert.Equal(t, reflect.Int64, field.Type.Kind(), "%s must use integer minor units", fieldName)
			}
		})
	}
}
