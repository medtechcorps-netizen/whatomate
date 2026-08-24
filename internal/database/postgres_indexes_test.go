package database

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetIndexesEnforcesOneActiveTransferPerContact(t *testing.T) {
	statements := strings.Join(getIndexes(), "\n")
	assert.Contains(t, statements, "ranked_active_transfers")
	assert.Contains(t, statements, "SET status = 'expired'")
	assert.Contains(t, statements, "uq_agent_transfers_org_contact_active")
	assert.Contains(t, statements, "WHERE status = 'active' AND deleted_at IS NULL")
}

func TestGetIndexesSupportsPerUserInboxAttentionSummary(t *testing.T) {
	statements := strings.Join(getIndexes(), "\n")
	assert.Contains(t, statements, "idx_messages_org_inbox_incoming_ingested")
	assert.Contains(t, statements, "NOT index_state.indisvalid")
	assert.Contains(t, statements, "must be dropped concurrently before retrying migration")
	assert.Contains(t, statements, "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_org_inbox_incoming_ingested")
	assert.Contains(t, statements, "messages(organization_id, inbox_conversation_id, (COALESCE(ingested_at, created_at)), id)")
	assert.Contains(t, statements, "inbox_conversation_id IS NOT NULL")
	assert.Contains(t, statements, "direction = 'incoming'")
	assert.Contains(t, statements, "deleted_at IS NULL")
}

func TestGetIndexesSupportsEffectiveIngestionTranscriptPagination(t *testing.T) {
	statements := strings.Join(getIndexes(), "\n")
	assert.Contains(t, statements, "idx_messages_org_inbox_ingested")
	assert.Contains(t, statements, "idx_messages_org_inbox_ingested_highwater")
	assert.Contains(t, statements, "idx_messages_org_contact_ingested")
	assert.Contains(t, statements, "idx_messages_org_contact_account_ingested")
	assert.Contains(t, statements, "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_org_inbox_ingested")
	assert.Contains(t, statements, "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_org_inbox_ingested_highwater")
	assert.Contains(t, statements, "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_org_contact_ingested")
	assert.Contains(t, statements, "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_messages_org_contact_account_ingested")
	assert.Contains(t, statements, "organization_id, inbox_conversation_id, (COALESCE(ingested_at, created_at)), id")
	assert.Contains(t, statements, "organization_id, inbox_conversation_id, ingested_at DESC")
	assert.Contains(t, statements, "WHERE ingested_at IS NOT NULL")
	assert.Contains(t, statements, "organization_id, contact_id, (COALESCE(ingested_at, created_at)), id")
	assert.Contains(t, statements, "organization_id, contact_id, whats_app_account, (COALESCE(ingested_at, created_at)), id")
	assert.Contains(t, statements, "invalid message ingestion-order pagination index")
}

func TestGetIndexesEnforcesOneOwnerForGloballyRoutedChannelIdentities(t *testing.T) {
	statements := strings.Join(getIndexes(), "\n")
	assert.Contains(t, statements, "uq_channel_accounts_global_routable_identity")
	assert.Contains(t, statements, "ON channel_accounts(channel, provider, external_account_id)")
	assert.Contains(t, statements, "channel IN ('instagram', 'messenger') AND provider = 'relay'")
	assert.Contains(t, statements, "channel = 'threads' AND provider = 'threads'")
}

func TestGetIndexesEnforcesManagedThreadsLiveClaimsAndTenantSecretBoundary(t *testing.T) {
	statements := strings.Join(getIndexes(), "\n")
	assert.Contains(t, statements, "chk_provider_integrations_management_mode")
	assert.Contains(t, statements, "management_mode = 'workspace_byo'")
	assert.Contains(t, statements, "management_mode = 'platform_managed'")
	assert.Contains(t, statements, "NOT (credential_data ?| ARRAY['app_secret', 'webhook_verify_token'])")
	assert.Contains(t, statements, "platform_app_key IS NOT NULL")
	assert.Contains(t, statements, "GROUP BY platform_app_id, oauth_subject_id")
	assert.Contains(t, statements, "GROUP BY platform_app_id, authority_asset_id")
	assert.Contains(t, statements, "uq_threads_platform_bindings_live_app_subject")
	assert.Contains(t, statements, "ON threads_platform_bindings(platform_app_id, oauth_subject_id)")
	assert.Contains(t, statements, "uq_threads_platform_bindings_live_app_asset")
	assert.Contains(t, statements, "ON threads_platform_bindings(platform_app_id, authority_asset_id)")
	assert.Contains(t, statements, "status IN ('pending', 'active', 'quarantined')")
	assert.Contains(t, statements, "cannot enforce managed Threads identity ownership")
}

func TestGetIndexesEnforcesOneOwnerForNormalizedLiveWhatsAppPhoneIDs(t *testing.T) {
	indexes := getIndexes()
	preflightPosition := -1
	normalizationPosition := -1
	indexPosition := -1
	legacyIndexPosition := -1
	for position, statement := range indexes {
		switch {
		case strings.Contains(statement, "cannot enforce unique live WhatsApp Phone IDs"):
			preflightPosition = position
			assert.Contains(t, statement, "WHERE deleted_at IS NULL")
			assert.Contains(t, statement, "GROUP BY BTRIM(phone_id)")
			assert.Contains(t, statement, "HAVING COUNT(*) > 1")
			assert.NotContains(t, statement, "UPDATE whatsapp_accounts")
			assert.NotContains(t, statement, "DELETE FROM whatsapp_accounts")
			assert.NotContains(t, statement, "array_agg")
		case strings.Contains(statement, "uq_whatsapp_accounts_live_phone_id"):
			indexPosition = position
			assert.Contains(t, statement, "CREATE UNIQUE INDEX")
			assert.Contains(t, statement, "ON whatsapp_accounts(BTRIM(phone_id))")
			assert.Contains(t, statement, "WHERE deleted_at IS NULL")
		case strings.Contains(statement, "SET phone_id = BTRIM(phone_id)"):
			normalizationPosition = position
			assert.Contains(t, statement, "WHERE phone_id <> BTRIM(phone_id)")
		case strings.Contains(statement, "idx_whatsapp_accounts_org_phone"):
			legacyIndexPosition = position
			assert.Contains(t, statement, "DROP INDEX IF EXISTS")
		}
	}

	assert.NotEqual(t, -1, preflightPosition)
	assert.NotEqual(t, -1, normalizationPosition)
	assert.NotEqual(t, -1, indexPosition)
	assert.NotEqual(t, -1, legacyIndexPosition)
	assert.Less(t, preflightPosition, normalizationPosition)
	assert.Less(t, normalizationPosition, indexPosition)
	assert.Less(t, preflightPosition, legacyIndexPosition)
}

func TestGetIndexesAllowsDisconnectedChannelAccountsToBeReconnected(t *testing.T) {
	statements := strings.Join(getIndexes(), "\n")
	assert.Contains(t, statements, "DROP INDEX IF EXISTS idx_channel_accounts_org_name")
	assert.Contains(t, statements, "DROP INDEX IF EXISTS idx_channel_accounts_external")
	assert.Contains(t, statements, "uq_channel_accounts_org_name_active")
	assert.Contains(t, statements, "uq_channel_accounts_org_external_active")
	assert.Contains(t, statements, "WHERE deleted_at IS NULL")

	globalUnique := strings.Index(statements, "uq_channel_accounts_global_routable_identity")
	activeNameUnique := strings.Index(statements, "uq_channel_accounts_org_name_active")
	activeExternalUnique := strings.Index(statements, "uq_channel_accounts_org_external_active")
	dropLegacyName := strings.Index(statements, "DROP INDEX IF EXISTS idx_channel_accounts_org_name")
	dropLegacyExternal := strings.Index(statements, "DROP INDEX IF EXISTS idx_channel_accounts_external")
	assert.Less(t, globalUnique, activeNameUnique)
	assert.Less(t, activeNameUnique, dropLegacyName)
	assert.Less(t, activeExternalUnique, dropLegacyExternal)
}
