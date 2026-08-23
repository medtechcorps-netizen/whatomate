package handlers

import (
	"context"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
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
	err := persistLegacyWhatsAppMessageMirror(a.DB, account, messageID)
	a.logLegacyWhatsAppMessageMirrorError(err, account, messageID)
}

// mirrorLegacyWhatsAppMessageInSavepoint preserves the live path's
// best-effort contract when the caller already owns a tenant transaction. A
// failed mirror must roll back to a savepoint; swallowing a PostgreSQL error
// directly would leave the entire request transaction aborted.
func (a *App) mirrorLegacyWhatsAppMessageInSavepoint(
	ctx context.Context,
	account *models.WhatsAppAccount,
	messageID uuid.UUID,
) {
	if a == nil || a.DB == nil || account == nil || messageID == uuid.Nil {
		return
	}
	current := bindRealtimeAppToDB(a.DB.WithContext(ctx), a)
	err := current.Transaction(func(tx *gorm.DB) error {
		return persistLegacyWhatsAppMessageMirror(tx, account, messageID)
	})
	a.logLegacyWhatsAppMessageMirrorError(err, account, messageID)
}

func persistLegacyWhatsAppMessageMirror(
	db *gorm.DB,
	account *models.WhatsAppAccount,
	messageID uuid.UUID,
) error {
	if db == nil || account == nil || messageID == uuid.Nil {
		return nil
	}
	_, err := channelapi.MirrorLegacyWhatsAppMessage(
		db,
		channelapi.LegacyMetaAccountRef{
			ID:             account.ID,
			OrganizationID: account.OrganizationID,
			Name:           account.Name,
			Status:         account.Status,
		},
		messageID,
	)
	return err
}

func (a *App) logLegacyWhatsAppMessageMirrorError(
	err error,
	account *models.WhatsAppAccount,
	messageID uuid.UUID,
) {
	if err != nil && a != nil && account != nil {
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
