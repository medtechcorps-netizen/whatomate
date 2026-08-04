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

func TestGetIndexesEnforcesOneOwnerForGloballyRoutedChannelIdentities(t *testing.T) {
	statements := strings.Join(getIndexes(), "\n")
	assert.Contains(t, statements, "uq_channel_accounts_global_routable_identity")
	assert.Contains(t, statements, "ON channel_accounts(channel, provider, external_account_id)")
	assert.Contains(t, statements, "channel IN ('instagram', 'messenger') AND provider = 'relay'")
	assert.Contains(t, statements, "channel = 'threads' AND provider = 'threads'")
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
