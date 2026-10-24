package handlers

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// stateSyncPhoneOwner creates a contact that owns phone under a BSUID other
// than the one a later Coexistence identity carries.
func stateSyncPhoneOwner(t *testing.T, env laneFNameEnv, phone, bsuid string) models.Contact {
	t.Helper()
	owner := models.Contact{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  env.account.OrganizationID,
		PhoneNumber:     phone,
		ProfileName:     "Phone Owner",
		WhatsAppAccount: env.account.Name,
		BSUID:           bsuid,
		Metadata:        models.JSONB{},
	}
	require.NoError(t, env.app.DB.Create(&owner).Error)
	return owner
}

func stateSyncContactByBSUID(t *testing.T, env laneFNameEnv, bsuid string) models.Contact {
	t.Helper()
	var contact models.Contact
	require.NoError(t, env.app.DB.First(&contact, "organization_id = ? AND bs_uid = ?", env.account.OrganizationID, bsuid).Error)
	return contact
}

// A state-sync add whose phone belongs to a contact with a different BSUID
// moves nothing onto that owner and notes the conflict on the BSUID's own
// contact. The add then writes that contact's metadata itself; the write must
// keep the note, which identity review relies on to keep the contact out of
// automatic routing.
func TestStateSyncAddKeepsCoexistencePhoneConflictNote(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()
	owner := stateSyncPhoneOwner(t, env, phone, "US.owner"+testutil.NewTestGraphObjectID()[:12])
	userID := "US.conflict" + testutil.NewTestGraphObjectID()[:12]

	// Meta first syncs the entry by BSUID only, then reveals the phone.
	laneFSync(t, env, laneFSyncItem{action: "add", userID: userID, fullName: "Saved Before Phone", at: laneFAt(10)})
	placeholder := stateSyncContactByBSUID(t, env, userID)
	require.NotEqual(t, owner.ID, placeholder.ID)

	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, userID: userID, fullName: "Saved With Phone", at: laneFAt(20)})

	stored := laneFContact(t, env, placeholder.ID)
	assert.True(t, isCoexistencePlaceholderPhone(stored.PhoneNumber), "the owner's phone is not moved onto the BSUID contact")
	assert.Equal(t, userID, stored.BSUID)
	assert.Equal(t, true, stored.Metadata["coexistence_phone_conflict"], "the add kept the conflict note")
	assert.Equal(t, normalizeCoexistencePhone(phone), stored.Metadata["coexistence_supplied_phone"])
	assert.Equal(t, owner.ID.String(), stored.Metadata["coexistence_phone_owner_contact_id"])
	assert.True(t, coexistenceIdentityMetadataNamesAnotherUser(&stored, userID, ""))
	// The add's own changes land on the same row.
	assert.Equal(t, true, stored.Metadata["coexistence_app_contact"])
	assert.Equal(t, laneFAt(20).Format(time.RFC3339), stored.Metadata["coexistence_app_contact_synced_at"])
	laneFAssertName(t, stored, "Saved With Phone", contactutil.ContactNameSourceWABusiness)

	ownerAfter := laneFContact(t, env, owner.ID)
	assert.Equal(t, owner.BSUID, ownerAfter.BSUID)
	assert.Equal(t, "Phone Owner", ownerAfter.ProfileName)
	assert.NotContains(t, ownerAfter.Metadata, "coexistence_app_contact")
}

// getOrCreateCoexistenceContact returns the conflicted contact with the note
// already in its in-memory metadata, so any caller that clones that metadata
// and writes it back keeps the note.
func TestCoexistencePhoneConflictReturnsNotedContact(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()
	owner := stateSyncPhoneOwner(t, env, phone, "US.owner"+testutil.NewTestGraphObjectID()[:12])
	userID := "US.conflict" + testutil.NewTestGraphObjectID()[:12]

	placeholder := laneFCoexistenceIdentity(t, env, coexistenceContactIdentity{UserID: userID})
	returned := laneFCoexistenceIdentity(t, env, coexistenceContactIdentity{UserID: userID, Phone: phone})
	require.Equal(t, placeholder.ID, returned.ID)

	stored := laneFContact(t, env, returned.ID)
	for _, key := range []string{
		"coexistence_phone_conflict",
		"coexistence_supplied_phone",
		"coexistence_phone_owner_contact_id",
	} {
		assert.Equal(t, stored.Metadata[key], returned.Metadata[key], key)
	}
	assert.Equal(t, true, returned.Metadata["coexistence_phone_conflict"])
	assert.Equal(t, owner.ID.String(), returned.Metadata["coexistence_phone_owner_contact_id"])
}

// The reconciliation note keeps the placeholder the caller holds equal to its
// row: metadata and the cleared bs_uid.
func TestCoexistenceReconciliationKeepsPlaceholderInStep(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()
	userID := "US.reconcile" + testutil.NewTestGraphObjectID()[:12]
	placeholder := laneFCoexistenceIdentity(t, env, coexistenceContactIdentity{UserID: userID})
	canonical := stateSyncPhoneOwner(t, env, phone, "")

	require.NoError(t, env.app.DB.Transaction(func(tx *gorm.DB) error {
		return env.app.scopedApp(tx, env.account.OrganizationID).
			noteCoexistenceContactReconciliation(&env.account, placeholder, &canonical, phone)
	}))

	stored := laneFContact(t, env, placeholder.ID)
	assert.Empty(t, stored.BSUID)
	assert.Equal(t, stored.BSUID, placeholder.BSUID)
	for _, key := range []string{
		"coexistence_reconciled_contact_id",
		"coexistence_reconciled_phone",
		"coexistence_phone_unavailable",
	} {
		assert.Equal(t, stored.Metadata[key], placeholder.Metadata[key], key)
	}
	assert.Equal(t, canonical.ID.String(), placeholder.Metadata["coexistence_reconciled_contact_id"])
	assert.Equal(t, false, placeholder.Metadata["coexistence_phone_unavailable"])
}

// Meta may redeliver or reorder address-book events. An add or remove older
// than the last applied add or removal leaves coexistence_app_contact and its
// times alone, with or without a saved name. Events in the same second apply
// in arrival order.
func TestStateSyncAppContactIgnoresOlderAddressBookEvents(t *testing.T) {
	env := laneFNewNameEnv(t, true)
	phone := laneFPhone()
	assertAppContact := func(msg string, appContact bool, syncedAt, removedAt time.Time) {
		t.Helper()
		stored := laneFContactByPhone(t, env, phone)
		assert.Equal(t, appContact, stored.Metadata["coexistence_app_contact"], msg)
		assert.Equal(t, syncedAt.Format(time.RFC3339), stored.Metadata["coexistence_app_contact_synced_at"], msg)
		if removedAt.IsZero() {
			assert.NotContains(t, stored.Metadata, "coexistence_app_contact_removed_at", msg)
		} else {
			assert.Equal(t, removedAt.Format(time.RFC3339), stored.Metadata["coexistence_app_contact_removed_at"], msg)
		}
	}

	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved", at: laneFAt(10)})
	laneFSync(t, env, laneFSyncItem{action: "remove", phone: phone, at: laneFAt(30)})
	assertAppContact("removed", false, laneFAt(10), laneFAt(30))

	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, fullName: "Saved", at: laneFAt(20)})
	assertAppContact("a redelivered add from before the removal", false, laneFAt(10), laneFAt(30))

	// The saved-name times do not move for an add without a name; the
	// app-contact rule uses its own times.
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, at: laneFAt(25)})
	assertAppContact("an older add without a name", false, laneFAt(10), laneFAt(30))

	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, at: laneFAt(50)})
	assertAppContact("a newer add", true, laneFAt(50), laneFAt(30))

	laneFSync(t, env, laneFSyncItem{action: "remove", phone: phone, at: laneFAt(40)})
	assertAppContact("a late removal from before the newer add", true, laneFAt(50), laneFAt(30))

	laneFSync(t, env, laneFSyncItem{action: "remove", phone: phone, at: laneFAt(50)})
	assertAppContact("a removal in the add's second", false, laneFAt(50), laneFAt(50))
	laneFSync(t, env, laneFSyncItem{action: "add", phone: phone, at: laneFAt(50)})
	assertAppContact("a re-add in the same second", true, laneFAt(50), laneFAt(50))

	stored := laneFContactByPhone(t, env, phone)
	laneFAssertName(t, stored, "Saved", contactutil.ContactNameSourceWABusiness)
}
