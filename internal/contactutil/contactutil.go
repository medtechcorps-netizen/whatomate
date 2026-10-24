package contactutil

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrCanonicalContactChanged asks a caller to retry its transaction. It means
// a merge changed the redirect chain between the initial read and the
// deterministic row-lock acquisition.
var ErrCanonicalContactChanged = errors.New("canonical contact changed while acquiring locks")

// ErrEmptyPhoneNumber rejects a contact lookup/create whose phone number is
// empty after normalization ("" or "+"). Contacts are unique per
// (organization, phone_number), so an empty phone would make every such
// caller share one contact row. Callers that only know a WhatsApp
// business-scoped user ID must resolve the contact by that identity instead.
var ErrEmptyPhoneNumber = errors.New("contact phone number is empty")

// IsEmptyPhone reports whether phoneNumber is empty once surrounding
// whitespace and one leading "+" are removed. GetOrCreateContact rejects such
// values with ErrEmptyPhoneNumber; request handlers use this to reject them
// with a validation error before any write.
func IsEmptyPhone(phoneNumber string) bool {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(phoneNumber), "+")) == ""
}

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
//   - Updates profile name if changed, unless a manual CRM name or a WhatsApp
//     Business saved name owns it (see ContactNameProtected)
//   - Handles race conditions on create by re-fetching
//   - Restores soft-deleted contacts if found
//
// Returns the contact, whether it was newly created, and any error. A phone
// that is empty after normalization returns ErrEmptyPhoneNumber.
func GetOrCreateContact(db *gorm.DB, orgID uuid.UUID, phoneNumber, profileName string) (*models.Contact, bool, error) {
	// Normalize phone number (remove + prefix if present)
	normalizedPhone := phoneNumber
	if len(normalizedPhone) > 0 && normalizedPhone[0] == '+' {
		normalizedPhone = normalizedPhone[1:]
	}
	if IsEmptyPhone(normalizedPhone) {
		return nil, false, ErrEmptyPhoneNumber
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
				if err := updateUnprotectedContactName(db, canonical, profileName); err != nil {
					return nil, false, err
				}
				return canonical, false, nil
			}
			if err := db.Unscoped().Model(&contact).Update("deleted_at", nil).Error; err != nil {
				return nil, false, err
			}
			contact.DeletedAt.Valid = false
		}
		// Update profile name if changed and no manual or saved name owns it
		if err := updateUnprotectedContactName(db, &contact, profileName); err != nil {
			return nil, false, err
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
				if err := updateUnprotectedContactName(db, canonical, profileName); err != nil {
					return nil, false, err
				}
				return canonical, false, nil
			}
			if err := db.Unscoped().Model(&contact).Update("deleted_at", nil).Error; err != nil {
				return nil, false, err
			}
			contact.DeletedAt.Valid = false
		}
		if err := updateUnprotectedContactName(db, &contact, profileName); err != nil {
			return nil, false, err
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
				if restoreErr := db.Unscoped().Model(&contact).Update("deleted_at", nil).Error; restoreErr != nil {
					return nil, false, restoreErr
				}
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

// Contact display-name provenance. contacts.profile_name stays the single
// display name; these contacts.metadata keys record where it came from so a
// lower-precedence source cannot overwrite it. Precedence, highest first:
// a manual CRM edit, the name saved in the WhatsApp Business app (Coexistence
// smb_app_state_sync), then the customer's own WhatsApp profile name or
// username. A contact named before provenance was recorded has no source.
const (
	ContactNameSourceKey        = "name_source"
	ContactNameSourceManual     = "manual"
	ContactNameSourceWABusiness = "wa_business"
	ContactNameSourceWAProfile  = "wa_profile"

	// ContactWAProfileNameKey holds the customer's latest WhatsApp profile name
	// or username, refreshed whatever owns the display name.
	ContactWAProfileNameKey = "wa_profile_name"
	// ContactWABusinessSavedNameKey holds the last name the business saved for
	// this customer in the WhatsApp Business app. A removal keeps it and sets
	// ContactWABusinessSavedNameRemovedAtKey.
	ContactWABusinessSavedNameKey = "wa_business_saved_name"
	// ContactWABusinessSavedNameAtKey is the saved name's ordering watermark:
	// the latest address-book event time (a Meta time, or the server's
	// now + 5 min cap for a future one) the saved-name state is known not to
	// predate. Older adds and removals are ignored. It is not necessarily the
	// time the current name was saved: an add without a time keeps it.
	ContactWABusinessSavedNameAtKey = "wa_business_saved_name_at"
	// ContactWABusinessSavedNameRemovedAtKey marks, by its presence, that the
	// contact was removed from the WhatsApp Business app; a non-empty value is
	// that removal's event time and orders later events.
	ContactWABusinessSavedNameRemovedAtKey = "wa_business_saved_name_removed_at"
)

// ContactNameSource returns the recorded source of a contact's display name,
// or "" when none is recorded.
func ContactNameSource(metadata models.JSONB) string {
	source, _ := metadata[ContactNameSourceKey].(string)
	return source
}

// ContactNameProtected reports whether a manual CRM edit or the WhatsApp
// Business saved name owns the display name, so a profile name, campaign
// recipient name or other observed name must not replace it.
func ContactNameProtected(metadata models.JSONB) bool {
	switch ContactNameSource(metadata) {
	case ContactNameSourceManual, ContactNameSourceWABusiness:
		return true
	default:
		return false
	}
}

// CloneContactMetadata returns a shallow copy of metadata that is safe to
// modify; it is never nil.
func CloneContactMetadata(metadata models.JSONB) models.JSONB {
	cloned := make(models.JSONB, len(metadata)+4)
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

// ApplyContactProfileName records name, the customer's own WhatsApp profile
// name or username, in metadata and returns the display name to store: name
// itself unless a manual or saved name owns the display name, in which case
// displayName is returned unchanged. metadata must be a copy the caller writes
// back; changed reports whether ApplyContactProfileName modified it.
func ApplyContactProfileName(displayName string, metadata models.JSONB, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || metadata == nil {
		return displayName, false
	}
	changed := false
	if current, _ := metadata[ContactWAProfileNameKey].(string); current != name {
		metadata[ContactWAProfileNameKey] = name
		changed = true
	}
	if ContactNameProtected(metadata) {
		return displayName, changed
	}
	if ContactNameSource(metadata) != ContactNameSourceWAProfile {
		metadata[ContactNameSourceKey] = ContactNameSourceWAProfile
		changed = true
	}
	return name, changed
}

// ObserveContactProfileName applies ApplyContactProfileName to a contact row
// the caller holds locked (FOR UPDATE) in its transaction. It returns the
// column updates to write to that row in the same transaction, or nil when
// nothing changes, and updates contact in memory to match.
func ObserveContactProfileName(contact *models.Contact, name string) map[string]any {
	if contact == nil || strings.TrimSpace(name) == "" {
		return nil
	}
	metadata := CloneContactMetadata(contact.Metadata)
	displayName, metadataChanged := ApplyContactProfileName(contact.ProfileName, metadata, name)
	updates := map[string]any{}
	if displayName != contact.ProfileName {
		updates["profile_name"] = displayName
		contact.ProfileName = displayName
	}
	if metadataChanged {
		updates["metadata"] = metadata
		contact.Metadata = metadata
	}
	if len(updates) == 0 {
		return nil
	}
	return updates
}

// ApplyManualContactName applies a CRM user's rename to metadata and returns
// the display name to store. A non-empty name that differs from displayName
// becomes the manual name. An empty name clears any manual name and falls back
// to the WhatsApp Business saved name, then the WhatsApp profile name.
// Re-submitting the current display name (an edit form saving other fields)
// keeps its source. metadata must be a copy the caller writes back; changed
// reports whether ApplyManualContactName modified it.
func ApplyManualContactName(displayName string, metadata models.JSONB, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if metadata == nil {
		return displayName, false
	}
	if name != "" {
		if name == displayName {
			return displayName, false
		}
		metadata[ContactNameSourceKey] = ContactNameSourceManual
		return name, true
	}
	if saved, _ := metadata[ContactWABusinessSavedNameKey].(string); strings.TrimSpace(saved) != "" {
		metadata[ContactNameSourceKey] = ContactNameSourceWABusiness
		return strings.TrimSpace(saved), true
	}
	if profile, _ := metadata[ContactWAProfileNameKey].(string); strings.TrimSpace(profile) != "" {
		metadata[ContactNameSourceKey] = ContactNameSourceWAProfile
		return strings.TrimSpace(profile), true
	}
	_, hadSource := metadata[ContactNameSourceKey]
	delete(metadata, ContactNameSourceKey)
	return "", hadSource
}

// updateUnprotectedContactName replaces contact's display name with name
// unless a manual or WhatsApp Business saved name owns it. The UPDATE itself
// re-checks the owner, so a rename committed after contact was read still
// wins over this name.
func updateUnprotectedContactName(db *gorm.DB, contact *models.Contact, name string) error {
	if name == "" || contact.ProfileName == name || ContactNameProtected(contact.Metadata) {
		return nil
	}
	result := db.Model(&models.Contact{}).
		Where("organization_id = ? AND id = ?", contact.OrganizationID, contact.ID).
		Where("COALESCE(metadata->>?, '') NOT IN ?",
			ContactNameSourceKey,
			[]string{ContactNameSourceManual, ContactNameSourceWABusiness}).
		Update("profile_name", name)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		contact.ProfileName = name
	}
	return nil
}
