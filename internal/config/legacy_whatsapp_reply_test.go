package config

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLegacyWhatsAppReplyRolloutGate(t *testing.T) {
	organizationID := uuid.NewString()
	otherOrganizationID := uuid.NewString()

	assert.False(t, (LegacyWhatsAppReplyConfig{}).OrganizationEnabled(organizationID))
	assert.False(t, (LegacyWhatsAppReplyConfig{Enabled: true}).OrganizationEnabled(organizationID))
	require.Error(t, ValidateLegacyWhatsAppReplyConfig(LegacyWhatsAppReplyConfig{Enabled: true}))
	allowlisted := LegacyWhatsAppReplyConfig{
		Enabled:                true,
		AllowedOrganizationIDs: organizationID,
	}
	assert.True(t, allowlisted.OrganizationEnabled(organizationID))
	assert.False(t, allowlisted.OrganizationEnabled(otherOrganizationID))
	require.NoError(t, ValidateLegacyWhatsAppReplyConfig(allowlisted))
	require.Error(t, ValidateLegacyWhatsAppReplyConfig(LegacyWhatsAppReplyConfig{
		AllowedOrganizationIDs: organizationID,
	}))
	require.Error(t, ValidateLegacyWhatsAppReplyConfig(LegacyWhatsAppReplyConfig{
		Enabled:                true,
		AllowedOrganizationIDs: "not-a-uuid",
	}))
}
