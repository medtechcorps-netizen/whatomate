package database

import "fmt"

type productTenantForeignKey struct {
	name        string
	childTable  string
	childCols   string
	parentTable string
	parentCols  string
	onDelete    string
}

// productIntegrityStatements adds database-level invariants for the product
// suite. Handler validation remains important for useful errors, but tenant and
// ledger correctness must not depend on every future write path remembering the
// same checks.
func productIntegrityStatements() []string {
	statements := []string{
		// Composite candidate keys let child rows bind both the UUID and tenant.
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_contacts_id_org ON contacts(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_billing_accounts_id_org ON billing_accounts(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_subscriptions_id_org ON subscriptions(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_invoices_id_org ON invoices(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_organization_onboardings_id_org ON organization_onboardings(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_provisioning_runs_id_org ON provisioning_runs(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_workspace_template_apps_id_org ON workspace_template_applications(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_consent_events_id_org ON consent_events(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_privacy_requests_id_org ON privacy_requests(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_support_cases_id_org ON support_cases(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_crm_pipelines_id_org ON crm_pipelines(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_crm_pipeline_stages_id_pipeline_org ON crm_pipeline_stages(id, pipeline_id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_crm_pipeline_stages_id_org ON crm_pipeline_stages(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_crm_leads_id_org ON crm_leads(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_crm_leads_org_idempotency ON crm_leads(organization_id, idempotency_key) WHERE idempotency_key <> '' AND deleted_at IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_follow_up_tasks_id_org ON follow_up_tasks(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_follow_up_tasks_org_idempotency ON follow_up_tasks(organization_id, idempotency_key) WHERE idempotency_key <> '' AND deleted_at IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_booking_services_id_org ON booking_services(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_booking_resources_id_org ON booking_resources(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_booking_events_id_org ON booking_events(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_bookings_id_org ON bookings(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_package_definitions_id_org ON package_definitions(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_package_entitlements_id_org ON package_entitlements(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_contact_packages_id_org ON contact_packages(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_contact_packages_id_contact_org ON contact_packages(id, contact_id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_credit_ledger_entries_id_org ON credit_ledger_entries(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_commerce_invoices_id_org ON commerce_invoices(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_commerce_invoices_id_contact_org ON commerce_invoices(id, contact_id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_provider_accounts_id_org ON payment_provider_accounts(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_provider_accounts_external ON payment_provider_accounts(organization_id, lower(provider), environment, lower(external_account_id))`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_intents_id_org ON payment_intents(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_transactions_id_org ON payment_transactions(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_transactions_provider_reference ON payment_transactions(organization_id, provider_account_id, lower(provider_transaction_id)) WHERE provider_transaction_id <> ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_copilot_runs_id_org ON copilot_runs(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_channel_accounts_id_org ON channel_accounts(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_contact_identities_id_org ON contact_identities(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_inbox_conversations_id_org ON inbox_conversations(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_conversation_participants_id_org ON conversation_participants(id, organization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_messages_id_org ON messages(id, organization_id)`,

		productCheckConstraint(
			"chk_booking_services_values",
			"booking_services",
			"duration_minutes > 0 AND buffer_before_mins >= 0 AND buffer_after_mins >= 0 AND default_capacity > 0 AND price_minor >= 0",
		),
		productCheckConstraint(
			"chk_availability_rules_values",
			"availability_rules",
			"weekday BETWEEN 0 AND 6 AND start_local_time < end_local_time AND (effective_until IS NULL OR effective_from IS NULL OR effective_until >= effective_from)",
		),
		productCheckConstraint(
			"chk_resource_time_off_interval",
			"resource_time_off",
			"ends_at > starts_at",
		),
		productCheckConstraint(
			"chk_booking_events_values",
			"booking_events",
			"ends_at > starts_at AND capacity > 0",
		),
		productCheckConstraint(
			"chk_bookings_quantity",
			"bookings",
			"quantity > 0",
		),
		productCheckConstraint(
			"chk_package_definitions_values",
			"package_definitions",
			"price_minor >= 0 AND validity_days > 0",
		),
		productCheckConstraint(
			"chk_package_entitlements_credits",
			"package_entitlements",
			"(is_unlimited = true AND credits >= 0) OR (is_unlimited = false AND credits > 0)",
		),
		productCheckConstraint(
			"chk_contact_packages_values",
			"contact_packages",
			"purchase_amount_minor >= 0 AND (expires_at IS NULL OR starts_at IS NULL OR expires_at > starts_at)",
		),
		productCheckConstraint(
			"chk_credit_balances_equation",
			"credit_balances",
			"granted >= 0 AND reserved >= 0 AND consumed >= 0 AND available >= 0 AND available = granted - reserved - consumed",
		),
		productCheckConstraint(
			"chk_credit_ledger_values",
			"credit_ledger_entries",
			"delta <> 0 AND balance_after >= 0",
		),
		productCheckConstraint(
			"chk_invoice_lines_equation",
			"invoice_lines",
			"quantity > 0 AND unit_amount_minor >= 0 AND subtotal_minor = quantity * unit_amount_minor AND tax_minor >= 0 AND total_minor = subtotal_minor + tax_minor",
		),
		productCheckConstraint(
			"chk_commerce_invoices_equation",
			"commerce_invoices",
			"subtotal_minor >= 0 AND discount_minor >= 0 AND discount_minor <= subtotal_minor AND tax_minor >= 0 AND total_minor >= 0 AND total_minor = subtotal_minor - discount_minor + tax_minor AND paid_minor >= 0 AND paid_minor <= total_minor AND due_minor >= 0 AND due_minor = total_minor - paid_minor",
		),
		productCheckConstraint(
			"chk_payment_intents_amount",
			"payment_intents",
			"amount_minor > 0",
		),
		productCheckConstraint(
			"chk_payment_transactions_amount",
			"payment_transactions",
			"amount_minor > 0",
		),
	}

	foreignKeys := []productTenantForeignKey{
		{"fk_subscriptions_billing_tenant", "subscriptions", "billing_account_id, organization_id", "billing_accounts", "id, organization_id", "RESTRICT"},
		{"fk_invoices_billing_tenant", "invoices", "billing_account_id, organization_id", "billing_accounts", "id, organization_id", "RESTRICT"},
		{"fk_invoices_subscription_tenant", "invoices", "subscription_id, organization_id", "subscriptions", "id, organization_id", "RESTRICT"},
		{"fk_usage_events_subscription_tenant", "usage_events", "subscription_id, organization_id", "subscriptions", "id, organization_id", "RESTRICT"},
		{"fk_usage_rollups_subscription_tenant", "billing_usage_rollups", "subscription_id, organization_id", "subscriptions", "id, organization_id", "RESTRICT"},
		{"fk_usage_rollups_invoice_tenant", "billing_usage_rollups", "invoice_id, organization_id", "invoices", "id, organization_id", "RESTRICT"},
		{"fk_provisioning_onboarding_tenant", "provisioning_runs", "onboarding_id, organization_id", "organization_onboardings", "id, organization_id", "CASCADE"},
		{"fk_template_apps_onboarding_tenant", "workspace_template_applications", "onboarding_id, organization_id", "organization_onboardings", "id, organization_id", "RESTRICT"},
		{"fk_template_apps_run_tenant", "workspace_template_applications", "provisioning_run_id, organization_id", "provisioning_runs", "id, organization_id", "RESTRICT"},
		{"fk_template_apps_previous_tenant", "workspace_template_applications", "previous_application_id, organization_id", "workspace_template_applications", "id, organization_id", "RESTRICT"},
		{"fk_template_maps_application_tenant", "workspace_template_resource_maps", "application_id, organization_id", "workspace_template_applications", "id, organization_id", "CASCADE"},
		{"fk_consent_events_contact_tenant", "consent_events", "contact_id, organization_id", "contacts", "id, organization_id", "RESTRICT"},
		{"fk_consent_states_contact_tenant", "consent_states", "contact_id, organization_id", "contacts", "id, organization_id", "RESTRICT"},
		{"fk_consent_states_event_tenant", "consent_states", "latest_event_id, organization_id", "consent_events", "id, organization_id", "RESTRICT"},
		{"fk_privacy_requests_contact_tenant", "privacy_requests", "contact_id, organization_id", "contacts", "id, organization_id", "RESTRICT"},
		{"fk_privacy_events_request_tenant", "privacy_request_events", "privacy_request_id, organization_id", "privacy_requests", "id, organization_id", "CASCADE"},
		{"fk_privacy_jobs_request_tenant", "privacy_jobs", "privacy_request_id, organization_id", "privacy_requests", "id, organization_id", "CASCADE"},
		{"fk_support_grants_case_tenant", "support_access_grants", "support_case_id, organization_id", "support_cases", "id, organization_id", "RESTRICT"},
		{"fk_recovery_checkpoints_case_tenant", "recovery_checkpoints", "support_case_id, organization_id", "support_cases", "id, organization_id", "RESTRICT"},
		{"fk_crm_stages_pipeline_tenant", "crm_pipeline_stages", "pipeline_id, organization_id", "crm_pipelines", "id, organization_id", "RESTRICT"},
		{"fk_crm_leads_contact_tenant", "crm_leads", "contact_id, organization_id", "contacts", "id, organization_id", "RESTRICT"},
		{"fk_crm_leads_pipeline_tenant", "crm_leads", "pipeline_id, organization_id", "crm_pipelines", "id, organization_id", "RESTRICT"},
		{"fk_crm_leads_stage_pipeline_tenant", "crm_leads", "stage_id, pipeline_id, organization_id", "crm_pipeline_stages", "id, pipeline_id, organization_id", "RESTRICT"},
		{"fk_crm_history_lead_tenant", "crm_stage_history", "lead_id, organization_id", "crm_leads", "id, organization_id", "CASCADE"},
		{"fk_crm_history_from_stage_tenant", "crm_stage_history", "from_stage_id, organization_id", "crm_pipeline_stages", "id, organization_id", "RESTRICT"},
		{"fk_crm_history_to_stage_tenant", "crm_stage_history", "to_stage_id, organization_id", "crm_pipeline_stages", "id, organization_id", "RESTRICT"},
		{"fk_tasks_contact_tenant", "follow_up_tasks", "contact_id, organization_id", "contacts", "id, organization_id", "RESTRICT"},
		{"fk_tasks_lead_tenant", "follow_up_tasks", "lead_id, organization_id", "crm_leads", "id, organization_id", "RESTRICT"},
		{"fk_tasks_booking_tenant", "follow_up_tasks", "booking_id, organization_id", "bookings", "id, organization_id", "RESTRICT"},
		{"fk_tasks_parent_tenant", "follow_up_tasks", "parent_task_id, organization_id", "follow_up_tasks", "id, organization_id", "RESTRICT"},
		{"fk_booking_service_links_service_tenant", "booking_service_resources", "service_id, organization_id", "booking_services", "id, organization_id", "CASCADE"},
		{"fk_booking_service_links_resource_tenant", "booking_service_resources", "resource_id, organization_id", "booking_resources", "id, organization_id", "CASCADE"},
		{"fk_availability_resource_tenant", "availability_rules", "resource_id, organization_id", "booking_resources", "id, organization_id", "CASCADE"},
		{"fk_time_off_resource_tenant", "resource_time_off", "resource_id, organization_id", "booking_resources", "id, organization_id", "CASCADE"},
		{"fk_booking_events_service_tenant", "booking_events", "service_id, organization_id", "booking_services", "id, organization_id", "RESTRICT"},
		{"fk_booking_events_resource_tenant", "booking_events", "resource_id, organization_id", "booking_resources", "id, organization_id", "RESTRICT"},
		{"fk_bookings_event_tenant", "bookings", "event_id, organization_id", "booking_events", "id, organization_id", "RESTRICT"},
		{"fk_bookings_contact_tenant", "bookings", "contact_id, organization_id", "contacts", "id, organization_id", "RESTRICT"},
		{"fk_bookings_contact_package_tenant", "bookings", "contact_package_id, contact_id, organization_id", "contact_packages", "id, contact_id, organization_id", "RESTRICT"},
		{"fk_package_entitlements_definition_tenant", "package_entitlements", "package_definition_id, organization_id", "package_definitions", "id, organization_id", "CASCADE"},
		{"fk_package_entitlements_service_tenant", "package_entitlements", "booking_service_id, organization_id", "booking_services", "id, organization_id", "RESTRICT"},
		{"fk_contact_packages_contact_tenant", "contact_packages", "contact_id, organization_id", "contacts", "id, organization_id", "RESTRICT"},
		{"fk_contact_packages_definition_tenant", "contact_packages", "package_definition_id, organization_id", "package_definitions", "id, organization_id", "RESTRICT"},
		{"fk_contact_packages_invoice_contact_tenant", "contact_packages", "invoice_id, contact_id, organization_id", "commerce_invoices", "id, contact_id, organization_id", "RESTRICT"},
		{"fk_credit_balances_package_tenant", "credit_balances", "contact_package_id, organization_id", "contact_packages", "id, organization_id", "CASCADE"},
		{"fk_credit_balances_entitlement_tenant", "credit_balances", "package_entitlement_id, organization_id", "package_entitlements", "id, organization_id", "RESTRICT"},
		{"fk_credit_ledger_package_tenant", "credit_ledger_entries", "contact_package_id, organization_id", "contact_packages", "id, organization_id", "RESTRICT"},
		{"fk_credit_ledger_entitlement_tenant", "credit_ledger_entries", "package_entitlement_id, organization_id", "package_entitlements", "id, organization_id", "RESTRICT"},
		{"fk_credit_ledger_booking_tenant", "credit_ledger_entries", "booking_id, organization_id", "bookings", "id, organization_id", "RESTRICT"},
		{"fk_credit_ledger_payment_tenant", "credit_ledger_entries", "payment_transaction_id, organization_id", "payment_transactions", "id, organization_id", "RESTRICT"},
		{"fk_credit_ledger_reversal_tenant", "credit_ledger_entries", "reversal_of_id, organization_id", "credit_ledger_entries", "id, organization_id", "RESTRICT"},
		{"fk_commerce_invoices_contact_tenant", "commerce_invoices", "contact_id, organization_id", "contacts", "id, organization_id", "RESTRICT"},
		{"fk_invoice_lines_invoice_tenant", "invoice_lines", "invoice_id, organization_id", "commerce_invoices", "id, organization_id", "CASCADE"},
		{"fk_invoice_lines_booking_tenant", "invoice_lines", "booking_id, organization_id", "bookings", "id, organization_id", "RESTRICT"},
		{"fk_invoice_lines_contact_package_tenant", "invoice_lines", "contact_package_id, organization_id", "contact_packages", "id, organization_id", "RESTRICT"},
		{"fk_invoice_lines_definition_tenant", "invoice_lines", "package_definition_id, organization_id", "package_definitions", "id, organization_id", "RESTRICT"},
		{"fk_payment_intents_provider_tenant", "payment_intents", "provider_account_id, organization_id", "payment_provider_accounts", "id, organization_id", "RESTRICT"},
		{"fk_payment_intents_invoice_contact_tenant", "payment_intents", "invoice_id, contact_id, organization_id", "commerce_invoices", "id, contact_id, organization_id", "RESTRICT"},
		{"fk_payment_transactions_provider_tenant", "payment_transactions", "provider_account_id, organization_id", "payment_provider_accounts", "id, organization_id", "RESTRICT"},
		{"fk_payment_transactions_intent_tenant", "payment_transactions", "intent_id, organization_id", "payment_intents", "id, organization_id", "RESTRICT"},
		{"fk_payment_transactions_invoice_tenant", "payment_transactions", "invoice_id, organization_id", "commerce_invoices", "id, organization_id", "RESTRICT"},
		{"fk_payment_transactions_original_tenant", "payment_transactions", "original_transaction_id, organization_id", "payment_transactions", "id, organization_id", "RESTRICT"},
		{"fk_payment_webhooks_provider_tenant", "payment_webhook_events", "provider_account_id, organization_id", "payment_provider_accounts", "id, organization_id", "RESTRICT"},
		{"fk_copilot_runs_contact_tenant", "copilot_runs", "contact_id, organization_id", "contacts", "id, organization_id", "RESTRICT"},
		{"fk_copilot_feedback_run_tenant", "copilot_feedback", "run_id, organization_id", "copilot_runs", "id, organization_id", "CASCADE"},
		{"fk_copilot_feedback_message_tenant", "copilot_feedback", "final_message_id, organization_id", "messages", "id, organization_id", "RESTRICT"},
		{"fk_channel_credentials_account_tenant", "channel_credentials", "channel_account_id, organization_id", "channel_accounts", "id, organization_id", "CASCADE"},
		{"fk_contact_identities_contact_tenant", "contact_identities", "contact_id, organization_id", "contacts", "id, organization_id", "CASCADE"},
		{"fk_contact_identities_account_tenant", "contact_identities", "channel_account_id, organization_id", "channel_accounts", "id, organization_id", "CASCADE"},
		{"fk_inbox_conversations_account_tenant", "inbox_conversations", "channel_account_id, organization_id", "channel_accounts", "id, organization_id", "RESTRICT"},
		{"fk_inbox_conversations_contact_tenant", "inbox_conversations", "contact_id, organization_id", "contacts", "id, organization_id", "RESTRICT"},
		{"fk_inbox_conversations_identity_tenant", "inbox_conversations", "contact_identity_id, organization_id", "contact_identities", "id, organization_id", "RESTRICT"},
		{"fk_conversation_participants_conversation_tenant", "conversation_participants", "conversation_id, organization_id", "inbox_conversations", "id, organization_id", "CASCADE"},
		{"fk_conversation_participants_identity_tenant", "conversation_participants", "contact_identity_id, organization_id", "contact_identities", "id, organization_id", "RESTRICT"},
		{"fk_conversation_reads_conversation_tenant", "conversation_reads", "conversation_id, organization_id", "inbox_conversations", "id, organization_id", "CASCADE"},
		{"fk_conversation_reads_participant_tenant", "conversation_reads", "participant_id, organization_id", "conversation_participants", "id, organization_id", "RESTRICT"},
		{"fk_conversation_reads_message_tenant", "conversation_reads", "last_read_message_id, organization_id", "messages", "id, organization_id", "RESTRICT"},
		{"fk_messages_contact_tenant", "messages", "contact_id, organization_id", "contacts", "id, organization_id", "RESTRICT"},
		{"fk_messages_inbox_conversation_tenant", "messages", "inbox_conversation_id, organization_id", "inbox_conversations", "id, organization_id", "RESTRICT"},
		{"fk_message_parts_message_tenant", "message_parts", "message_id, organization_id", "messages", "id, organization_id", "CASCADE"},
		{"fk_message_parts_conversation_tenant", "message_parts", "conversation_id, organization_id", "inbox_conversations", "id, organization_id", "CASCADE"},
		{"fk_inbound_events_account_tenant", "inbound_events", "channel_account_id, organization_id", "channel_accounts", "id, organization_id", "RESTRICT"},
		{"fk_message_events_account_tenant", "message_events", "channel_account_id, organization_id", "channel_accounts", "id, organization_id", "RESTRICT"},
		{"fk_message_events_conversation_tenant", "message_events", "conversation_id, organization_id", "inbox_conversations", "id, organization_id", "CASCADE"},
		{"fk_message_events_message_tenant", "message_events", "message_id, organization_id", "messages", "id, organization_id", "RESTRICT"},
		{"fk_outbox_jobs_account_tenant", "outbox_jobs", "channel_account_id, organization_id", "channel_accounts", "id, organization_id", "RESTRICT"},
		{"fk_outbox_jobs_conversation_tenant", "outbox_jobs", "conversation_id, organization_id", "inbox_conversations", "id, organization_id", "CASCADE"},
		{"fk_outbox_jobs_message_tenant", "outbox_jobs", "message_id, organization_id", "messages", "id, organization_id", "RESTRICT"},
		{"fk_contact_preferences_contact_tenant", "contact_channel_preferences", "contact_id, organization_id", "contacts", "id, organization_id", "CASCADE"},
		{"fk_contact_preferences_account_tenant", "contact_channel_preferences", "channel_account_id, organization_id", "channel_accounts", "id, organization_id", "CASCADE"},
		{"fk_contact_preferences_identity_tenant", "contact_channel_preferences", "contact_identity_id, organization_id", "contact_identities", "id, organization_id", "RESTRICT"},
	}
	for _, foreignKey := range foreignKeys {
		statements = append(statements, productTenantForeignKeyStatement(foreignKey))
	}
	return statements
}

func productCheckConstraint(name, table, expression string) string {
	return fmt.Sprintf(`DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = '%s') THEN
			ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s);
		END IF;
	END $$`, name, table, name, expression)
}

func productTenantForeignKeyStatement(foreignKey productTenantForeignKey) string {
	return fmt.Sprintf(`DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = '%s') THEN
			ALTER TABLE %s
			ADD CONSTRAINT %s
			FOREIGN KEY (%s)
			REFERENCES %s(%s)
			ON DELETE %s;
		END IF;
	END $$`,
		foreignKey.name,
		foreignKey.childTable,
		foreignKey.name,
		foreignKey.childCols,
		foreignKey.parentTable,
		foreignKey.parentCols,
		foreignKey.onDelete,
	)
}
