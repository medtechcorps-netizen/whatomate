package whatsappaccount

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const StatusActive = "active"

var ErrOutboundInactive = errors.New("WhatsApp account is not active for outbound messaging")

// RequireActiveForOutbound is the shared fail-closed status check for every
// legacy WhatsApp send surface. Pending registration/subscription states and a
// disconnected Coexistence account are intentionally non-sendable.
func RequireActiveForOutbound(account *models.WhatsAppAccount) error {
	if account == nil || strings.TrimSpace(account.Status) != StatusActive {
		return ErrOutboundInactive
	}
	return nil
}

// LockActiveForOutbound serializes the final provider decision with account
// lifecycle updates. A PARTNER_REMOVED or ACCOUNT_OFFBOARDED update committed
// first prevents the send; a lifecycle update arriving during a provider call
// waits for that already-started attempt to finish.
func LockActiveForOutbound(
	tx *gorm.DB,
	organizationID, accountID uuid.UUID,
) error {
	_, err := LockAndLoadActiveForOutbound(tx, organizationID, accountID)
	return err
}

// LockAndLoadActiveForOutbound returns the current account row under the same
// shared lock that fences the provider call. Callers must build provider
// credentials from this returned row, never from a pre-lock cache or request
// snapshot, so a completed credential rotation cannot be followed by a send
// using the superseded token.
func LockAndLoadActiveForOutbound(
	tx *gorm.DB,
	organizationID, accountID uuid.UUID,
) (*models.WhatsAppAccount, error) {
	if tx == nil || organizationID == uuid.Nil || accountID == uuid.Nil {
		return nil, ErrOutboundInactive
	}

	var stored models.WhatsAppAccount
	err := tx.Clauses(clause.Locking{Strength: "SHARE"}).
		Where("id = ? AND organization_id = ?", accountID, organizationID).
		First(&stored).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrOutboundInactive
	}
	if err != nil {
		return nil, fmt.Errorf("lock WhatsApp account for outbound messaging: %w", err)
	}
	if err := RequireActiveForOutbound(&stored); err != nil {
		return nil, err
	}
	return &stored, nil
}
