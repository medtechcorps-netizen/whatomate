package handlers

import (
	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
)

// mirrorLegacyWhatsAppMessage is deliberately best-effort on live delivery
// paths. The established WhatsApp send/webhook flow remains authoritative and
// the idempotent migration backfill repairs a transient mirror failure.
func (a *App) mirrorLegacyWhatsAppMessage(
	account *models.WhatsAppAccount,
	messageID uuid.UUID,
) {
	if account == nil || messageID == uuid.Nil {
		return
	}
	_, err := channelapi.MirrorLegacyWhatsAppMessage(
		a.DB,
		channelapi.LegacyMetaAccountRef{
			ID:             account.ID,
			OrganizationID: account.OrganizationID,
			Name:           account.Name,
			Status:         account.Status,
		},
		messageID,
	)
	if err != nil {
		a.Log.Error(
			"Failed to mirror legacy WhatsApp message into omnichannel inbox",
			"error",
			err,
			"organization_id",
			account.OrganizationID,
			"message_id",
			messageID,
		)
	}
}

func (a *App) mirrorLegacyWhatsAppRead(
	organizationID uuid.UUID,
	contactID uuid.UUID,
) {
	if err := channelapi.MarkLegacyWhatsAppConversationRead(
		a.DB,
		organizationID,
		contactID,
	); err != nil {
		a.Log.Error(
			"Failed to mirror legacy WhatsApp read state into omnichannel inbox",
			"error",
			err,
			"organization_id",
			organizationID,
			"contact_id",
			contactID,
		)
	}
}
