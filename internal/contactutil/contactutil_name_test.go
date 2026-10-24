package contactutil

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestLaneFApplyContactProfileName(t *testing.T) {
	t.Run("unowned display name follows the profile", func(t *testing.T) {
		metadata := models.JSONB{}
		displayName, changed := ApplyContactProfileName("Old", metadata, " Ali WA ")
		assert.Equal(t, "Ali WA", displayName)
		assert.True(t, changed)
		assert.Equal(t, "Ali WA", metadata[ContactWAProfileNameKey])
		assert.Equal(t, ContactNameSourceWAProfile, metadata[ContactNameSourceKey])

		displayName, changed = ApplyContactProfileName("Ali WA", metadata, "Ali WA")
		assert.Equal(t, "Ali WA", displayName)
		assert.False(t, changed, "an unchanged profile name writes nothing")
	})

	for _, source := range []string{ContactNameSourceManual, ContactNameSourceWABusiness} {
		t.Run(source+" name is kept and the profile is recorded", func(t *testing.T) {
			metadata := models.JSONB{ContactNameSourceKey: source}
			displayName, changed := ApplyContactProfileName("Owned", metadata, "Ali WA")
			assert.Equal(t, "Owned", displayName)
			assert.True(t, changed)
			assert.Equal(t, "Ali WA", metadata[ContactWAProfileNameKey])
			assert.Equal(t, source, metadata[ContactNameSourceKey])
		})
	}

	t.Run("empty name changes nothing", func(t *testing.T) {
		metadata := models.JSONB{}
		displayName, changed := ApplyContactProfileName("Old", metadata, "  ")
		assert.Equal(t, "Old", displayName)
		assert.False(t, changed)
		assert.Empty(t, metadata)
	})
}

func TestLaneFObserveContactProfileName(t *testing.T) {
	stored := models.JSONB{"crm": "kept"}
	contact := &models.Contact{ProfileName: "Old", Metadata: stored}
	updates := ObserveContactProfileName(contact, "Ali WA")
	require.NotNil(t, updates)
	assert.Equal(t, "Ali WA", updates["profile_name"])
	assert.Equal(t, "Ali WA", contact.ProfileName)
	assert.Equal(t, "kept", contact.Metadata["crm"])
	assert.NotContains(t, stored, ContactWAProfileNameKey, "the stored map is never mutated")

	assert.Nil(t, ObserveContactProfileName(contact, "Ali WA"), "a repeat writes nothing")

	contact.Metadata[ContactNameSourceKey] = ContactNameSourceManual
	contact.ProfileName = "Dr Ali"
	updates = ObserveContactProfileName(contact, "Ali New")
	require.NotNil(t, updates)
	assert.NotContains(t, updates, "profile_name")
	assert.Equal(t, "Dr Ali", contact.ProfileName)
	assert.Equal(t, "Ali New", contact.Metadata[ContactWAProfileNameKey])
}

func TestLaneFApplyManualContactName(t *testing.T) {
	t.Run("a changed name becomes manual", func(t *testing.T) {
		metadata := models.JSONB{ContactNameSourceKey: ContactNameSourceWABusiness}
		displayName, changed := ApplyManualContactName("Saved", metadata, " Dr Ali ")
		assert.Equal(t, "Dr Ali", displayName)
		assert.True(t, changed)
		assert.Equal(t, ContactNameSourceManual, metadata[ContactNameSourceKey])
	})
	t.Run("re-submitting the current name keeps its source", func(t *testing.T) {
		metadata := models.JSONB{ContactNameSourceKey: ContactNameSourceWABusiness}
		displayName, changed := ApplyManualContactName("Saved", metadata, "Saved")
		assert.Equal(t, "Saved", displayName)
		assert.False(t, changed)
		assert.Equal(t, ContactNameSourceWABusiness, metadata[ContactNameSourceKey])
	})
	t.Run("clearing falls back to the saved name", func(t *testing.T) {
		metadata := models.JSONB{
			ContactNameSourceKey:          ContactNameSourceManual,
			ContactWABusinessSavedNameKey: "Saved",
			ContactWAProfileNameKey:       "Profile",
		}
		displayName, changed := ApplyManualContactName("Dr Ali", metadata, "")
		assert.Equal(t, "Saved", displayName)
		assert.True(t, changed)
		assert.Equal(t, ContactNameSourceWABusiness, metadata[ContactNameSourceKey])
	})
	t.Run("clearing falls back to the profile name", func(t *testing.T) {
		metadata := models.JSONB{ContactNameSourceKey: ContactNameSourceManual, ContactWAProfileNameKey: "Profile"}
		displayName, _ := ApplyManualContactName("Dr Ali", metadata, " ")
		assert.Equal(t, "Profile", displayName)
		assert.Equal(t, ContactNameSourceWAProfile, metadata[ContactNameSourceKey])
	})
	t.Run("clearing with nothing to fall back to empties the name", func(t *testing.T) {
		metadata := models.JSONB{ContactNameSourceKey: ContactNameSourceManual}
		displayName, changed := ApplyManualContactName("Dr Ali", metadata, "")
		assert.Equal(t, "", displayName)
		assert.True(t, changed)
		assert.NotContains(t, metadata, ContactNameSourceKey)
	})
}

func laneFNameTestContact(t *testing.T, db *gorm.DB, name string, metadata models.JSONB) (models.Organization, models.Contact) {
	t.Helper()
	uid := uuid.New().String()[:8]
	org := models.Organization{BaseModel: models.BaseModel{ID: uuid.New()}, Name: "lanef-" + uid, Slug: "lanef-" + uid}
	require.NoError(t, db.Create(&org).Error)
	contact := models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		PhoneNumber:    "6011" + uid,
		ProfileName:    name,
		Metadata:       metadata,
	}
	require.NoError(t, db.Create(&contact).Error)
	return org, contact
}

func laneFReloadContact(t *testing.T, db *gorm.DB, id uuid.UUID) models.Contact {
	t.Helper()
	var contact models.Contact
	require.NoError(t, db.Unscoped().First(&contact, "id = ?", id).Error)
	return contact
}

// A campaign recipient name (or any other caller of GetOrCreateContact) never
// replaces a manual or WhatsApp Business saved name, but still names a contact
// whose name no one owns.
func TestLaneFGetOrCreateContactKeepsOwnedNames(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for _, source := range []string{ContactNameSourceManual, ContactNameSourceWABusiness} {
		t.Run(source, func(t *testing.T) {
			org, stored := laneFNameTestContact(t, db, "Owned Name", models.JSONB{ContactNameSourceKey: source})
			contact, created, err := GetOrCreateContact(db, org.ID, stored.PhoneNumber, "Campaign Name")
			require.NoError(t, err)
			assert.False(t, created)
			assert.Equal(t, "Owned Name", contact.ProfileName)
			assert.Equal(t, "Owned Name", laneFReloadContact(t, db, stored.ID).ProfileName)
		})
	}
	t.Run("profile name", func(t *testing.T) {
		org, stored := laneFNameTestContact(t, db, "Profile", models.JSONB{ContactNameSourceKey: ContactNameSourceWAProfile})
		contact, _, err := GetOrCreateContact(db, org.ID, stored.PhoneNumber, "Campaign Name")
		require.NoError(t, err)
		assert.Equal(t, "Campaign Name", contact.ProfileName)
		reloaded := laneFReloadContact(t, db, stored.ID)
		assert.Equal(t, "Campaign Name", reloaded.ProfileName)
		assert.Equal(t, ContactNameSourceWAProfile, reloaded.Metadata[ContactNameSourceKey])
	})
	t.Run("JSON null metadata", func(t *testing.T) {
		org, stored := laneFNameTestContact(t, db, "Legacy", nil)
		require.NoError(t, db.Exec("UPDATE contacts SET metadata = 'null'::jsonb WHERE id = ?", stored.ID).Error)
		_, _, err := GetOrCreateContact(db, org.ID, stored.PhoneNumber, "Campaign Name")
		require.NoError(t, err)
		assert.Equal(t, "Campaign Name", laneFReloadContact(t, db, stored.ID).ProfileName)
	})
}

// A campaign worker reads the contact without a row lock. A manual rename that
// commits after that read, while the worker waits for the row, must still win:
// the UPDATE re-checks the owner on the row it finally locks.
func TestLaneFGetOrCreateContactManualRenameCommittedWhileWaitingWins(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org, stored := laneFNameTestContact(t, db, "Profile", models.JSONB{})

	renameTx := db.Begin()
	require.NoError(t, renameTx.Error)
	t.Cleanup(func() { _ = renameTx.Rollback().Error })
	var locked models.Contact
	require.NoError(t, renameTx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, "id = ?", stored.ID).Error)
	require.NoError(t, renameTx.Model(&models.Contact{}).Where("id = ?", stored.ID).Updates(map[string]any{
		"profile_name": "Dr Ali",
		"metadata":     models.JSONB{ContactNameSourceKey: ContactNameSourceManual},
	}).Error)

	type result struct {
		contact *models.Contact
		err     error
	}
	done := make(chan result, 1)
	go func() {
		var out result
		out.err = db.Transaction(func(tx *gorm.DB) error {
			var err error
			out.contact, _, err = GetOrCreateContact(tx, org.ID, stored.PhoneNumber, "Campaign Name")
			return err
		})
		done <- out
	}()

	laneFAwaitLockWaiter(t, db)
	require.NoError(t, renameTx.Commit().Error)
	out := <-done
	require.NoError(t, out.err)
	reloaded := laneFReloadContact(t, db, stored.ID)
	assert.Equal(t, "Dr Ali", reloaded.ProfileName)
	assert.Equal(t, ContactNameSourceManual, reloaded.Metadata[ContactNameSourceKey])
}

// laneFAwaitLockWaiter waits until some session of this database is blocked
// on a lock, so a test can release the lock holder at a known point.
func laneFAwaitLockWaiter(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting int64
		if err := db.Raw(
			"SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'",
		).Scan(&waiting).Error; err != nil {
			return false
		}
		return waiting > 0
	}, 10*time.Second, 10*time.Millisecond, "the concurrent writer never waited for the row lock")
}
