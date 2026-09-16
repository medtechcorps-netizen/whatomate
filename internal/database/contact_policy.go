package database

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
)

const (
	AutomaticReplyBlockedHumanHandover = "human_handover_active"
	AutomaticReplyBlockedIdentityHold  = "identity_review_hold"
)

// ContactAutomaticReplyPolicy is a fail-closed, physical-contact policy
// projection. Callers must treat a returned error as blocked; an unavailable
// reader is never permission to contact an AI or delivery provider.
type ContactAutomaticReplyPolicy struct {
	Allowed bool
	Reason  string
}

// LockOrganizationPolicyScope is the common tenant-wide policy mutation and
// admission mutex. It must be acquired before account, contact, session, job,
// or outbox row locks.
func LockOrganizationPolicyScope(tx *gorm.DB, organizationID uuid.UUID) error {
	if tx == nil || organizationID == uuid.Nil {
		return errors.New("tenant organization policy transaction is required")
	}
	if err := tx.Exec(
		"SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(?, 0))",
		WhatsAppIdentityReviewContactSelectorFenceKey(organizationID),
	).Error; err != nil {
		return fmt.Errorf("lock contact selector policy fence: %w", err)
	}
	return lockOrganizationPolicyScope(tx, organizationID, "UPDATE")
}

// LockOrganizationAIAttemptScope is the per-physical-attempt fence. KEY SHARE
// conflicts with policy writers taking UPDATE while remaining compatible with
// the platform-compliance write guard's SHARE lock and ordinary FK checks.
func LockOrganizationAIAttemptScope(tx *gorm.DB, organizationID uuid.UUID) error {
	return lockOrganizationPolicyScope(tx, organizationID, "KEY SHARE")
}

func lockOrganizationPolicyScope(
	tx *gorm.DB,
	organizationID uuid.UUID,
	strength string,
) error {
	if tx == nil || organizationID == uuid.Nil {
		return errors.New("tenant organization policy transaction is required")
	}
	var organization struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	result := tx.Raw(
		fmt.Sprintf(`SELECT id
		   FROM organizations
		  WHERE id = ?
		    AND deleted_at IS NULL
		  FOR %s`, strength),
		organizationID,
	).Scan(&organization)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 || organization.ID == uuid.Nil {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// RequireIndependentAIAttemptConnections rejects a one-connection pool: the
// outer organization guard and the independently committed action/result
// ledger must use two physical connections. MaxOpenConns==0 is unbounded.
func RequireIndependentAIAttemptConnections(db *gorm.DB) error {
	if db == nil {
		return errors.New("AI attempt fence requires a database")
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("inspect AI attempt connection pool: %w", err)
	}
	if maximum := sqlDB.Stats().MaxOpenConnections; maximum > 0 && maximum < 2 {
		return fmt.Errorf("AI attempt fence requires at least two database connections (configured %d)", maximum)
	}
	return nil
}

// EvaluateContactAutomaticReplyPolicy evaluates every physical-contact block.
// It deliberately does not infer routing or transfer ownership. The caller
// owns the organization attempt/policy fence for the duration of the decision.
func EvaluateContactAutomaticReplyPolicy(
	tx *gorm.DB,
	organizationID, contactID uuid.UUID,
) (ContactAutomaticReplyPolicy, error) {
	blocked := ContactAutomaticReplyPolicy{Allowed: false}
	if tx == nil || organizationID == uuid.Nil || contactID == uuid.Nil {
		return blocked, errors.New("automatic reply policy requires tenant and contact identity")
	}

	var activeTransfers int64
	if err := tx.Model(&models.AgentTransfer{}).
		Where(
			"organization_id = ? AND contact_id = ? AND status = ?",
			organizationID,
			contactID,
			models.TransferStatusActive,
		).
		Count(&activeTransfers).Error; err != nil {
		return blocked, fmt.Errorf("read active handover policy: %w", err)
	}
	if activeTransfers > 0 {
		blocked.Reason = AutomaticReplyBlockedHumanHandover
		return blocked, nil
	}

	hasIdentityHold, err := ContactHasBlockingIdentityReviewHold(
		tx,
		organizationID,
		contactID,
	)
	if err != nil {
		return blocked, fmt.Errorf("read identity-review hold policy: %w", err)
	}
	if hasIdentityHold {
		blocked.Reason = AutomaticReplyBlockedIdentityHold
		return blocked, nil
	}

	return ContactAutomaticReplyPolicy{Allowed: true}, nil
}

// LockContactPolicyScope serializes policy changes that can revoke an
// automatic reply with the worker's final provider-dispatch fence. Callers
// must invoke it inside a transaction and hold the transaction open through
// the policy write or dispatch-fence update.
func LockContactPolicyScope(
	tx *gorm.DB,
	organizationID, contactID uuid.UUID,
) error {
	if tx == nil {
		return errors.New("contact policy lock requires a database transaction")
	}
	if organizationID == uuid.Nil || contactID == uuid.Nil {
		return errors.New("contact policy lock requires organization and contact IDs")
	}

	var contact struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	result := tx.Raw(
		`SELECT id
		   FROM contacts
		  WHERE id = ?
		    AND organization_id = ?
		    AND deleted_at IS NULL
		  FOR UPDATE`,
		contactID,
		organizationID,
	).Scan(&contact)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 || contact.ID == uuid.Nil {
		return gorm.ErrRecordNotFound
	}
	return nil
}
