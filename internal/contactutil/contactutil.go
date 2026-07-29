package contactutil

import (
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrCanonicalContactChanged asks a caller to retry its transaction. It means
// a merge changed the redirect chain between the initial read and the
// deterministic row-lock acquisition.
var ErrCanonicalContactChanged = errors.New("canonical contact changed while acquiring locks")

// ResolveCanonicalContact follows a soft-deleted merge alias to its active
// canonical contact. A bounded traversal rejects corrupt cycles.
func ResolveCanonicalContact(
	db *gorm.DB,
	orgID, contactID uuid.UUID,
) (*models.Contact, error) {
	currentID := contactID
	visited := map[uuid.UUID]struct{}{}
	for depth := 0; depth < 16; depth++ {
		if _, exists := visited[currentID]; exists {
			return nil, errors.New("contact merge redirect contains a cycle")
		}
		visited[currentID] = struct{}{}
		var contact models.Contact
		if err := db.Unscoped().
			Where("id = ? AND organization_id = ?", currentID, orgID).
			First(&contact).Error; err != nil {
			return nil, err
		}
		if contact.MergedIntoID == nil {
			if contact.DeletedAt.Valid {
				return nil, gorm.ErrRecordNotFound
			}
			return &contact, nil
		}
		currentID = *contact.MergedIntoID
	}
	return nil, fmt.Errorf("contact merge redirect exceeds maximum depth")
}

// ResolveCanonicalContactForUpdate resolves a contact merge redirect while
// locking every row in the observed path in deterministic UUID order.
//
// The caller must run this inside a transaction. Reading the path before
// taking locks lets us use the same ordering as the merge transaction. Once
// locked, the path is revalidated; a concurrent merge that changed it causes
// ErrCanonicalContactChanged so the caller can roll back and retry instead of
// writing through a stale contact ID.
func ResolveCanonicalContactForUpdate(
	tx *gorm.DB,
	orgID, contactID uuid.UUID,
) (*models.Contact, error) {
	path, err := contactMergePath(tx, orgID, contactID)
	if err != nil {
		return nil, err
	}

	pathIDs := make([]uuid.UUID, 0, len(path))
	for _, contact := range path {
		pathIDs = append(pathIDs, contact.ID)
	}
	sort.Slice(pathIDs, func(i, j int) bool {
		return pathIDs[i].String() < pathIDs[j].String()
	})

	var locked []models.Contact
	if err := tx.Unscoped().
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("organization_id = ? AND id IN ?", orgID, pathIDs).
		Order("id").
		Find(&locked).Error; err != nil {
		return nil, err
	}
	if len(locked) != len(pathIDs) {
		return nil, ErrCanonicalContactChanged
	}

	lockedByID := make(map[uuid.UUID]models.Contact, len(locked))
	for _, contact := range locked {
		lockedByID[contact.ID] = contact
	}

	currentID := contactID
	visited := make(map[uuid.UUID]struct{}, len(locked))
	for depth := 0; depth < 16; depth++ {
		if _, exists := visited[currentID]; exists {
			return nil, errors.New("contact merge redirect contains a cycle")
		}
		visited[currentID] = struct{}{}

		contact, exists := lockedByID[currentID]
		if !exists {
			return nil, ErrCanonicalContactChanged
		}
		if contact.MergedIntoID == nil {
			if contact.DeletedAt.Valid {
				return nil, gorm.ErrRecordNotFound
			}
			return &contact, nil
		}
		if _, exists := lockedByID[*contact.MergedIntoID]; !exists {
			return nil, ErrCanonicalContactChanged
		}
		currentID = *contact.MergedIntoID
	}
	return nil, fmt.Errorf("contact merge redirect exceeds maximum depth")
}

func contactMergePath(
	db *gorm.DB,
	orgID, contactID uuid.UUID,
) ([]models.Contact, error) {
	path := make([]models.Contact, 0, 2)
	currentID := contactID
	visited := map[uuid.UUID]struct{}{}
	for depth := 0; depth < 16; depth++ {
		if _, exists := visited[currentID]; exists {
			return nil, errors.New("contact merge redirect contains a cycle")
		}
		visited[currentID] = struct{}{}

		var contact models.Contact
		if err := db.Unscoped().
			Where("id = ? AND organization_id = ?", currentID, orgID).
			First(&contact).Error; err != nil {
			return nil, err
		}
		path = append(path, contact)
		if contact.MergedIntoID == nil {
			if contact.DeletedAt.Valid {
				return nil, gorm.ErrRecordNotFound
			}
			return path, nil
		}
		currentID = *contact.MergedIntoID
	}
	return nil, fmt.Errorf("contact merge redirect exceeds maximum depth")
}

// GetOrCreateContact finds or creates a contact for the given phone number.
// Merges behaviors from both handler and worker implementations:
//   - Normalizes phone (strips leading "+")
//   - Tries both normalized and +prefix forms
//   - Updates profile name if changed
//   - Handles race conditions on create by re-fetching
//   - Restores soft-deleted contacts if found
//
// Returns the contact, whether it was newly created, and any error.
func GetOrCreateContact(db *gorm.DB, orgID uuid.UUID, phoneNumber, profileName string) (*models.Contact, bool, error) {
	// Normalize phone number (remove + prefix if present)
	normalizedPhone := phoneNumber
	if len(normalizedPhone) > 0 && normalizedPhone[0] == '+' {
		normalizedPhone = normalizedPhone[1:]
	}

	// Try to find existing contact with normalized phone (including soft-deleted)
	var contact models.Contact
	if err := db.Unscoped().Where("organization_id = ? AND phone_number = ?", orgID, normalizedPhone).First(&contact).Error; err == nil {
		// Restore if soft-deleted
		if contact.DeletedAt.Valid {
			if contact.MergedIntoID != nil {
				canonical, resolveErr := ResolveCanonicalContact(db, orgID, contact.ID)
				if resolveErr != nil {
					return nil, false, resolveErr
				}
				if profileName != "" && canonical.ProfileName != profileName {
					if err := db.Model(canonical).Update("profile_name", profileName).Error; err != nil {
						return nil, false, err
					}
					canonical.ProfileName = profileName
				}
				return canonical, false, nil
			}
			db.Unscoped().Model(&contact).Update("deleted_at", nil)
			contact.DeletedAt.Valid = false
		}
		// Update profile name if changed
		if profileName != "" && contact.ProfileName != profileName {
			db.Model(&contact).Update("profile_name", profileName)
		}
		return &contact, false, nil
	}

	// Also try with + prefix (contacts may have been stored with it)
	if err := db.Unscoped().Where("organization_id = ? AND phone_number = ?", orgID, "+"+normalizedPhone).First(&contact).Error; err == nil {
		// Restore if soft-deleted
		if contact.DeletedAt.Valid {
			if contact.MergedIntoID != nil {
				canonical, resolveErr := ResolveCanonicalContact(db, orgID, contact.ID)
				if resolveErr != nil {
					return nil, false, resolveErr
				}
				if profileName != "" && canonical.ProfileName != profileName {
					if err := db.Model(canonical).Update("profile_name", profileName).Error; err != nil {
						return nil, false, err
					}
					canonical.ProfileName = profileName
				}
				return canonical, false, nil
			}
			db.Unscoped().Model(&contact).Update("deleted_at", nil)
			contact.DeletedAt.Valid = false
		}
		if profileName != "" && contact.ProfileName != profileName {
			db.Model(&contact).Update("profile_name", profileName)
		}
		return &contact, false, nil
	}

	// Create new contact
	contact = models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		PhoneNumber:    normalizedPhone,
		ProfileName:    profileName,
	}
	if err := db.Create(&contact).Error; err != nil {
		// Race condition: another goroutine may have created the contact
		if err2 := db.Unscoped().Where("organization_id = ? AND phone_number = ?", orgID, normalizedPhone).First(&contact).Error; err2 == nil {
			// Restore if soft-deleted
			if contact.DeletedAt.Valid {
				if contact.MergedIntoID != nil {
					canonical, resolveErr := ResolveCanonicalContact(db, orgID, contact.ID)
					if resolveErr != nil {
						return nil, false, resolveErr
					}
					return canonical, false, nil
				}
				db.Unscoped().Model(&contact).Update("deleted_at", nil)
				contact.DeletedAt.Valid = false
			}
			return &contact, false, nil
		}
		return nil, false, err
	}
	return &contact, true, nil
}

// FindContact finds a contact for the given phone number with both forms (normalized and +prefix).
func FindContact(db *gorm.DB, orgID uuid.UUID, phoneNumber string) (*models.Contact, error) {
	normalizedPhone := phoneNumber
	if len(normalizedPhone) > 0 && normalizedPhone[0] == '+' {
		normalizedPhone = normalizedPhone[1:]
	}

	var contact models.Contact
	if err := db.Where("organization_id = ? AND phone_number = ?", orgID, normalizedPhone).First(&contact).Error; err == nil {
		return &contact, nil
	}

	if err := db.Where("organization_id = ? AND phone_number = ?", orgID, "+"+normalizedPhone).First(&contact).Error; err == nil {
		return &contact, nil
	}

	return nil, gorm.ErrRecordNotFound
}
