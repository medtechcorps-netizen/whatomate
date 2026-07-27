package database

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProductIntegrityStatementsCoverTenantAndLedgerBoundaries(t *testing.T) {
	t.Parallel()

	statements := strings.Join(productIntegrityStatements(), "\n")
	for _, required := range []string{
		"fk_crm_leads_stage_pipeline_tenant",
		"fk_booking_events_resource_tenant",
		"fk_bookings_contact_package_tenant",
		"fk_contact_packages_invoice_contact_tenant",
		"fk_payment_intents_invoice_contact_tenant",
		"fk_payment_transactions_original_tenant",
		"uq_crm_leads_org_idempotency",
		"uq_follow_up_tasks_org_idempotency",
		"uq_payment_provider_accounts_external",
		"uq_payment_transactions_provider_reference",
		"chk_availability_rules_values",
		"chk_resource_time_off_interval",
		"chk_booking_events_values",
		"chk_package_definitions_values",
		"chk_credit_balances_equation",
		"chk_commerce_invoices_equation",
		"chk_invoice_lines_equation",
		"chk_payment_transactions_amount",
	} {
		assert.Contains(t, statements, required)
	}
	assert.NotContains(t, statements, "ON DELETE SET NULL",
		"composite nullable tenant foreign keys use RESTRICT to preserve tenant binding")
}

func TestProductIntegrityForeignKeyStatementsAreNamed(t *testing.T) {
	t.Parallel()

	for _, statement := range productIntegrityStatements() {
		require.NotContains(t, statement, "%!",
			"formatting errors must not reach migration SQL")
		if strings.Contains(statement, "ADD CONSTRAINT") {
			assert.Contains(t, statement, "pg_constraint")
		}
	}
}
