package whatsappaccount

import (
	"testing"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequireActiveForOutbound(t *testing.T) {
	for _, test := range []struct {
		name    string
		account *models.WhatsAppAccount
		allowed bool
	}{
		{name: "active", account: &models.WhatsAppAccount{Status: "active"}, allowed: true},
		{name: "active with storage whitespace", account: &models.WhatsAppAccount{Status: " active "}, allowed: true},
		{name: "reconnected remains disconnected", account: &models.WhatsAppAccount{Status: "disconnected"}},
		{name: "pending registration", account: &models.WhatsAppAccount{Status: "pending_registration"}},
		{name: "pending subscription", account: &models.WhatsAppAccount{Status: "pending_subscription"}},
		{name: "subscription failed", account: &models.WhatsAppAccount{Status: "subscription_failed"}},
		{name: "missing account"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := RequireActiveForOutbound(test.account)
			if test.allowed {
				require.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, ErrOutboundInactive)
		})
	}
}
