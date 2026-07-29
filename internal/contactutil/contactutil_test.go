package contactutil

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetOrCreateContact_CreatesNew(t *testing.T) {
	db := testutil.SetupTestDB(t)
	uid := uuid.New().String()[:8]
	org := models.Organization{BaseModel: models.BaseModel{ID: uuid.New()}, Name: "test-" + uid, Slug: "test-" + uid}
	require.NoError(t, db.Create(&org).Error)

	contact, isNew, err := GetOrCreateContact(db, org.ID, "1234567890", "Alice")
	require.NoError(t, err)
	assert.True(t, isNew)
	assert.Equal(t, "1234567890", contact.PhoneNumber)
	assert.Equal(t, "Alice", contact.ProfileName)
}

func TestGetOrCreateContact_FindsExisting(t *testing.T) {
	db := testutil.SetupTestDB(t)
	uid := uuid.New().String()[:8]
	org := models.Organization{BaseModel: models.BaseModel{ID: uuid.New()}, Name: "test-" + uid, Slug: "test-" + uid}
	require.NoError(t, db.Create(&org).Error)

	existing := models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		PhoneNumber:    "1234567890",
		ProfileName:    "Alice",
	}
	require.NoError(t, db.Create(&existing).Error)

	contact, isNew, err := GetOrCreateContact(db, org.ID, "1234567890", "Alice")
	require.NoError(t, err)
	assert.False(t, isNew)
	assert.Equal(t, existing.ID, contact.ID)
}

func TestGetOrCreateContact_NormalizesPlus(t *testing.T) {
	db := testutil.SetupTestDB(t)
	uid := uuid.New().String()[:8]
	org := models.Organization{BaseModel: models.BaseModel{ID: uuid.New()}, Name: "test-" + uid, Slug: "test-" + uid}
	require.NoError(t, db.Create(&org).Error)

	existing := models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		PhoneNumber:    "1234567890",
		ProfileName:    "Bob",
	}
	require.NoError(t, db.Create(&existing).Error)

	contact, isNew, err := GetOrCreateContact(db, org.ID, "+1234567890", "Bob")
	require.NoError(t, err)
	assert.False(t, isNew)
	assert.Equal(t, existing.ID, contact.ID)
}

func TestGetOrCreateContact_FindsPlusPrefix(t *testing.T) {
	db := testutil.SetupTestDB(t)
	uid := uuid.New().String()[:8]
	org := models.Organization{BaseModel: models.BaseModel{ID: uuid.New()}, Name: "test-" + uid, Slug: "test-" + uid}
	require.NoError(t, db.Create(&org).Error)

	existing := models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		PhoneNumber:    "+1234567890",
		ProfileName:    "Charlie",
	}
	require.NoError(t, db.Create(&existing).Error)

	contact, isNew, err := GetOrCreateContact(db, org.ID, "1234567890", "Charlie")
	require.NoError(t, err)
	assert.False(t, isNew)
	assert.Equal(t, existing.ID, contact.ID)
}

func TestGetOrCreateContact_UpdatesProfileName(t *testing.T) {
	db := testutil.SetupTestDB(t)
	uid := uuid.New().String()[:8]
	org := models.Organization{BaseModel: models.BaseModel{ID: uuid.New()}, Name: "test-" + uid, Slug: "test-" + uid}
	require.NoError(t, db.Create(&org).Error)

	existing := models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		PhoneNumber:    "1234567890",
		ProfileName:    "Old Name",
	}
	require.NoError(t, db.Create(&existing).Error)

	contact, isNew, err := GetOrCreateContact(db, org.ID, "1234567890", "New Name")
	require.NoError(t, err)
	assert.False(t, isNew)

	var reloaded models.Contact
	require.NoError(t, db.First(&reloaded, contact.ID).Error)
	assert.Equal(t, "New Name", reloaded.ProfileName)
}

func TestGetOrCreateContact_RoutesMergedAliasToCanonicalWithoutRestoring(t *testing.T) {
	db := testutil.SetupTestDB(t)
	uid := uuid.New().String()[:8]
	org := models.Organization{
		BaseModel: models.BaseModel{ID: uuid.New()},
		Name:      "test-" + uid,
		Slug:      "test-" + uid,
	}
	require.NoError(t, db.Create(&org).Error)

	canonical := models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		PhoneNumber:    "601100000001",
		ProfileName:    "Canonical",
	}
	alias := models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		PhoneNumber:    "601100000002",
		ProfileName:    "Duplicate",
	}
	require.NoError(t, db.Create(&canonical).Error)
	require.NoError(t, db.Create(&alias).Error)
	now := time.Now().UTC()
	require.NoError(t, db.Model(&alias).Updates(map[string]any{
		"merged_into_id": canonical.ID,
		"merged_at":      now,
		"deleted_at":     now,
	}).Error)

	resolved, isNew, err := GetOrCreateContact(
		db,
		org.ID,
		alias.PhoneNumber,
		"Latest profile",
	)
	require.NoError(t, err)
	require.False(t, isNew)
	require.Equal(t, canonical.ID, resolved.ID)
	require.Equal(t, "Latest profile", resolved.ProfileName)

	var storedAlias models.Contact
	require.NoError(t, db.Unscoped().First(&storedAlias, alias.ID).Error)
	require.True(t, storedAlias.DeletedAt.Valid)
	require.NotNil(t, storedAlias.MergedIntoID)
	require.Equal(t, canonical.ID, *storedAlias.MergedIntoID)
}
