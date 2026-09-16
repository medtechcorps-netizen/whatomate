package handlers

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/whatsappaccount"
	"gorm.io/gorm"
)

// withLockedWhatsAppAccountForOutbound holds the account lifecycle lock across
// one Graph mutation and supplies credentials from the locked row. Under RLS,
// request handlers already own the tenant transaction, so reuse it rather than
// attempting to open a second tenant transaction that could wait on itself.
func (a *App) withLockedWhatsAppAccountForOutbound(
	ctx context.Context,
	organizationID, accountID uuid.UUID,
	write func(*models.WhatsAppAccount) error,
) error {
	if a == nil || write == nil || organizationID == uuid.Nil || accountID == uuid.Nil {
		return whatsappaccount.ErrOutboundInactive
	}
	if ctx == nil {
		ctx = context.Background()
	}
	run := func(tx *gorm.DB) error {
		account, err := whatsappaccount.LockAndLoadActiveForOutbound(tx, organizationID, accountID)
		if err != nil {
			return err
		}
		if err := a.prepareWhatsAppAccountForRuntime(account); err != nil {
			return err
		}
		return write(account)
	}

	if a.rlsEnabled() && a.hasTenantScope() {
		if a.tenantOrgID != organizationID || a.DB == nil {
			return errors.New("outbound WhatsApp account tenant scope does not match organization")
		}
		return run(a.DB.WithContext(ctx))
	}
	return a.outgoingTenantTransaction(ctx, organizationID, run)
}
