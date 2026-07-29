package database

import (
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

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
