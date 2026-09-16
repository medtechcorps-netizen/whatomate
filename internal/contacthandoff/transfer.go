// Package contacthandoff owns transaction-local staff attention and durable AI cutoffs.
package contacthandoff

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

var ErrActiveTransferExists = errors.New("contact already has an active transfer")

const (
	SuppressionKey         = "automatic_ai_suppressed"
	ProposalInvalidatedKey = "ai_booking_handoff_invalidated"
	CutoffKey              = "ai_booking_handoff_cutoff"
)

// CreateActiveTx preserves the existing row primitive. Caller must supply the
// canonical contact and already-validated actor/assignment; no transaction is
// started here. Do not call under a separate physical AI attempt connection.
func CreateActiveTx(tx *gorm.DB, transfer *models.AgentTransfer) error {
	if tx == nil || transfer == nil {
		return errors.New("complete agent transfer transaction state is required")
	}
	if err := database.LockOrganizationPolicyScope(tx, transfer.OrganizationID); err != nil {
		return err
	}
	if err := database.LockContactPolicyScope(tx, transfer.OrganizationID, transfer.ContactID); err != nil {
		return err
	}
	var count int64
	if err := tx.Model(&models.AgentTransfer{}).Where("organization_id = ? AND contact_id = ? AND status = ?", transfer.OrganizationID, transfer.ContactID, models.TransferStatusActive).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return ErrActiveTransferExists
	}
	return tx.Create(transfer).Error
}

type Request struct {
	OrganizationID, ContactID uuid.UUID
	AccountName, Reason       string
	Source                    models.TransferSource
}
type Result struct {
	Transfer models.AgentTransfer
	Contact  models.Contact
	Created  bool
}

// HandoffTx is deliberately local and error-returning. Organization policy is
// acquired FIRST (or reentered on this same transaction), before canonical
// contact/session writes. It must run only after any physical model/send fence
// has been released. No provider call, broadcast, commit or fabricated staff.
func HandoffTx(tx *gorm.DB, in Request) (Result, error) {
	var out Result
	if tx == nil || in.OrganizationID == uuid.Nil || in.ContactID == uuid.Nil || strings.TrimSpace(in.AccountName) == "" || len(in.AccountName) > 100 || len(in.Reason) > 2000 {
		return out, errors.New("invalid handoff scope")
	}
	if in.Source == "" {
		in.Source = models.TransferSourceFlow
	}
	if err := database.LockOrganizationPolicyScope(tx, in.OrganizationID); err != nil {
		return out, err
	}
	contact, err := contactutil.ResolveCanonicalContactForUpdate(tx, in.OrganizationID, in.ContactID)
	if err != nil {
		return out, err
	}
	out.Contact = *contact
	err = tx.Where("organization_id = ? AND contact_id = ? AND status = ?", in.OrganizationID, contact.ID, models.TransferStatusActive).First(&out.Transfer).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		out.Transfer = models.AgentTransfer{BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: in.OrganizationID, ContactID: contact.ID, WhatsAppAccount: in.AccountName, PhoneNumber: contact.PhoneNumber, Status: models.TransferStatusActive, Source: in.Source, Notes: strings.TrimSpace(in.Reason), TransferredAt: time.Now().UTC()}
		if err := CreateActiveTx(tx, &out.Transfer); err != nil {
			return out, err
		}
		out.Created = true
	} else if err != nil {
		return out, err
	}
	if err := CutoffTx(tx, in.OrganizationID, contact.ID); err != nil {
		return out, err
	}
	if err := tx.Where("id = ? AND organization_id = ?", contact.ID, in.OrganizationID).First(&out.Contact).Error; err != nil {
		return out, err
	}
	return out, nil
}

// CutoffTx requires the caller's organization policy and canonical contact
// locks. It preserves action/provider ledgers and consumed proposal receipts.
// Old admitted turns remain suppressed even after the transfer is resumed.
func CutoffTx(tx *gorm.DB, orgID, contactID uuid.UUID) error {
	if tx == nil || orgID == uuid.Nil || contactID == uuid.Nil {
		return errors.New("invalid handoff cutoff")
	}
	now := time.Now().UTC()
	var contact models.Contact
	if err := tx.Select("id", "metadata").Where("id = ? AND organization_id = ?", contactID, orgID).First(&contact).Error; err != nil {
		return err
	}
	if raw, exists := contact.Metadata[CutoffKey]; exists {
		value, ok := raw.(string)
		if !ok {
			return errors.New("invalid prior handoff cutoff")
		}
		prior, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return err
		}
		if !now.After(prior) {
			now = prior.Add(time.Nanosecond)
		}
	}
	patch, err := json.Marshal(models.JSONB{SuppressionKey: true, "automatic_ai_suppression_reason": database.AutomaticReplyBlockedHumanHandover, "automatic_ai_suppressed_at": now.Format(time.RFC3339Nano)})
	if err != nil {
		return err
	}
	admitted := tx.Model(&models.ScheduledJob{}).Select("aggregate_id").Where("organization_id = ? AND kind IN ? AND aggregate_type = ?", orgID, []string{"inbound_message.continuation", models.ScheduledJobKindChannelAIReply}, "message")
	if err := tx.Model(&models.Message{}).Where("organization_id = ? AND contact_id = ? AND direction = ?", orgID, contactID, models.DirectionIncoming).Where("id IN (?)", admitted).Where("metadata->'inbound_continuation_completed' IS DISTINCT FROM 'true'::jsonb").Where("metadata->? IS DISTINCT FROM 'true'::jsonb", SuppressionKey).Update("metadata", gorm.Expr("COALESCE(metadata, '{}'::jsonb) || ?::jsonb", string(patch))).Error; err != nil {
		return err
	}
	epoch, _ := json.Marshal(models.JSONB{CutoffKey: now.Format(time.RFC3339Nano)})
	update := tx.Model(&models.Contact{}).Where("id = ? AND organization_id = ?", contactID, orgID).Updates(map[string]any{"chatbot_last_message_at": nil, "chatbot_reminder_sent": false, "metadata": gorm.Expr("COALESCE(metadata, '{}'::jsonb) || ?::jsonb", string(epoch))})
	if update.Error != nil {
		return update.Error
	}
	if update.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	invalidation, _ := json.Marshal(models.JSONB{ProposalInvalidatedKey: true})
	// Ordinary non-key UPDATE is compatible with the outer continuation's FK
	// KEY SHARE. Never upgrade to FOR UPDATE across that second connection.
	if err := tx.Model(&models.ChatbotSession{}).Where("organization_id = ? AND contact_id = ? AND status = ?", orgID, contactID, models.SessionStatusActive).Updates(map[string]any{"status": models.SessionStatusCancelled, "completed_at": now, "session_data": gorm.Expr("COALESCE(session_data, '{}'::jsonb) || ?::jsonb", string(invalidation))}).Error; err != nil {
		return err
	}
	return tx.Model(&models.InboxConversation{}).Where("organization_id = ? AND contact_id = ?", orgID, contactID).Update("metadata", gorm.Expr("COALESCE(metadata, '{}'::jsonb) || ?::jsonb", string(invalidation))).Error
}

// MessageSuppressed is a read-only projection, not an authority/lock helper.
func MessageSuppressed(message *models.Message) bool {
	if message == nil {
		return true
	}
	value, exists := message.Metadata[SuppressionKey]
	if !exists {
		return false
	}
	suppressed, valid := value.(bool)
	return !valid || suppressed
}
func SuppressedTx(tx *gorm.DB, orgID, messageID uuid.UUID) (bool, error) {
	if tx == nil || orgID == uuid.Nil || messageID == uuid.Nil {
		return true, errors.New("invalid inbound suppression scope")
	}
	var message models.Message
	if err := tx.Select("id", "contact_id", "created_at", "ingested_at", "metadata").Where("id = ? AND organization_id = ? AND direction = ?", messageID, orgID, models.DirectionIncoming).First(&message).Error; err != nil {
		return true, err
	}
	if MessageSuppressed(&message) {
		return true, nil
	}
	var contact models.Contact
	if err := tx.Select("id", "metadata", "merged_into_id").Where("id = ? AND organization_id = ?", message.ContactID, orgID).First(&contact).Error; err != nil {
		return true, err
	}
	if contact.MergedIntoID != nil {
		return true, nil
	}
	if raw, exists := contact.Metadata[CutoffKey]; exists {
		value, ok := raw.(string)
		if !ok {
			return true, errors.New("invalid handoff cutoff")
		}
		cutoff, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return true, err
		}
		// EffectiveIngestedAt uses server persistence order, not the provider's
		// display timestamp. A late job for an old inbound cannot revive after Resume.
		if !message.EffectiveIngestedAt().After(cutoff) {
			return true, nil
		}
	}
	return false, nil
}
