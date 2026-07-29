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
