package customeractivity

import (
	"errors"
	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"testing"
	"time"
)

func TestPureActivityPayloadProtectsAuthority(t *testing.T) {
	event := models.CustomerActivityEvent{ID: uuid.New(), ContactID: uuid.New(), EventType: models.CustomerActivityBookingCreated, Category: models.CustomerActivityCategoryBooking, ActorType: models.CustomerActivityActorSystem, OccurredAt: time.Now().UTC(), Title: "Reserved", Metadata: models.JSONB{"source": "ai"}}
	payload := OutboxPayload(event, models.JSONB{"contact_id": "forged", " ACTOR_TYPE ": "staff", "Outbox_Event_ID": "forged", "METADATA": "forged", " ": "bad", "extra": "allowed"})
	require.Equal(t, event.ContactID.String(), payload["contact_id"])
	require.Equal(t, string(models.CustomerActivityActorSystem), payload["actor_type"])
	require.Equal(t, "", payload["actor_user_id"])
	require.Equal(t, event.Metadata, payload["metadata"])
	require.NotContains(t, payload, "Outbox_Event_ID")
	require.NotContains(t, payload, "ACTOR_TYPE")
	require.Equal(t, "allowed", payload["extra"])
	_, err := RecordTx(nil, uuid.New(), Input{})
	require.Error(t, err)
}

func TestActivityCallerTransactionRollbackAndIdempotency(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	contact := testutil.CreateTestContact(t, db, org.ID)
	source := uuid.New()
	input := Input{ContactID: contact.ID, EventType: models.CustomerActivityBookingCreated, Category: models.CustomerActivityCategoryBooking, Title: " Booking reserved ", SourceObjectType: "booking", SourceObjectID: &source, IdempotencyKey: uuid.NewString()}
	stopped := errors.New("confirmation intent failed")
	require.ErrorIs(t, db.Transaction(func(tx *gorm.DB) error {
		event, err := RecordTx(tx, org.ID, input)
		require.NoError(t, err)
		require.Equal(t, models.CustomerActivityActorSystem, event.ActorType)
		require.Nil(t, event.ActorUserID)
		return stopped
	}), stopped)
	for _, model := range []any{&models.CustomerActivityEvent{}, &models.OutboxEvent{}} {
		var count int64
		require.NoError(t, db.Model(model).Where("organization_id = ?", org.ID).Count(&count).Error)
		require.Zero(t, count)
	}
	var original *models.CustomerActivityEvent
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error { var err error; original, err = RecordTx(tx, org.ID, input); return err }))
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		event, err := RecordTx(tx, org.ID, input)
		require.NoError(t, err)
		require.Equal(t, original.ID, event.ID)
		return nil
	}))
	for _, model := range []any{&models.CustomerActivityEvent{}, &models.OutboxEvent{}} {
		var count int64
		require.NoError(t, db.Model(model).Where("organization_id = ?", org.ID).Count(&count).Error)
		require.EqualValues(t, 1, count)
	}
	different := input
	different.ContactID = testutil.CreateTestContact(t, db, org.ID).ID
	require.Error(t, db.Transaction(func(tx *gorm.DB) error { _, err := RecordTx(tx, org.ID, different); return err }))
	// The same key in another tenant is an independent event, never a replay
	// of a different tenant's receipt. Caller supplies that tenant's contact.
	other := testutil.CreateTestOrganization(t, db)
	different.ContactID = testutil.CreateTestContact(t, db, other.ID).ID
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		event, err := RecordTx(tx, other.ID, different)
		if err == nil {
			require.NotEqual(t, original.ID, event.ID)
			require.Equal(t, other.ID, event.OrganizationID)
		}
		return err
	}))
}
