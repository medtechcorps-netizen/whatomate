package channel

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

func TestValidateOutboundPurposeRestrictsMarketing(t *testing.T) {
	t.Parallel()

	require.NoError(t, ValidateOutboundPurpose(models.ChannelPreferencePurposeService))
	require.NoError(t, ValidateOutboundPurpose(models.ChannelPreferencePurposeTransactional))
	assert.Error(t, ValidateOutboundPurpose(models.ChannelPreferencePurposeMarketing))
	assert.Error(t, ValidateOutboundPurpose(""))
}

func TestValidateServiceWindowRequiresInitiationTemplate(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	expired := now.Add(-time.Minute)
	capabilities := Capabilities{
		ServiceWindow:      true,
		Templates:          true,
		BusinessInitiation: true,
	}
	assert.Error(t, ValidateServiceWindow(
		capabilities,
		&expired,
		[]MessagePart{{Type: models.MessagePartTypeText, Text: "Hello"}},
		now,
	))
	require.NoError(t, ValidateServiceWindow(
		capabilities,
		&expired,
		[]MessagePart{{Type: models.MessagePartTypeTemplate}},
		now,
	))
}

func TestOutboundConsentAllowedCanonicalNegativeWinsAcrossSubjects(t *testing.T) {
	tests := []struct {
		name       string
		action     models.ConsentAction
		status     models.ConsentStatus
		wantReason string
	}{
		{
			name:       "denied",
			action:     models.ConsentActionDenied,
			status:     models.ConsentStatusDenied,
			wantReason: "consent_denied",
		},
		{
			name:       "withdrawn",
			action:     models.ConsentActionWithdrawn,
			status:     models.ConsentStatusWithdrawn,
			wantReason: "consent_withdrawn",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			organization := testutil.CreateTestOrganization(t, db)
			canonical := testutil.CreateTestContact(t, db, organization.ID)
			alias := testutil.CreateTestContact(t, db, organization.ID)
			mergedAt := time.Now().UTC()
			require.NoError(t, db.Unscoped().Model(alias).Updates(map[string]any{
				"merged_into_id": canonical.ID,
				"merged_at":      mergedAt,
				"deleted_at":     mergedAt,
			}).Error)

			negativeAt := mergedAt.Add(-time.Hour)
			createChannelConsentState(
				t,
				db,
				organization.ID,
				canonical.ID,
				"phone:"+canonical.PhoneNumber,
				tt.action,
				tt.status,
				negativeAt,
			)
			createChannelConsentState(
				t,
				db,
				organization.ID,
				canonical.ID,
				"contact:"+canonical.ID.String(),
				models.ConsentActionGranted,
				models.ConsentStatusGranted,
				mergedAt,
			)

			allowed, reason, err := OutboundConsentAllowed(
				db,
				organization.ID,
				alias.ID,
				uuid.New(),
				models.ChannelWhatsApp,
				models.ChannelPreferencePurposeService,
				mergedAt.Add(time.Minute),
			)
			require.NoError(t, err)
			assert.False(t, allowed)
			assert.Equal(t, tt.wantReason, reason)
		})
	}
}

func TestOutboundConsentAllowedWaitsForConcurrentMergeStateMove(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	canonical := testutil.CreateTestContact(t, db, organization.ID)
	source := testutil.CreateTestContact(t, db, organization.ID)
	now := time.Now().UTC()
	createChannelConsentState(
		t,
		db,
		organization.ID,
		source.ID,
		"contact:"+source.ID.String(),
		models.ConsentActionDenied,
		models.ConsentStatusDenied,
		now.Add(-time.Hour),
	)

	mergeTx := db.Begin()
	require.NoError(t, mergeTx.Error)
	t.Cleanup(func() {
		_ = mergeTx.Rollback().Error
	})
	var locked []models.Contact
	require.NoError(t, mergeTx.Unscoped().
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("organization_id = ? AND id IN ?", organization.ID, []uuid.UUID{
			canonical.ID,
			source.ID,
		}).
		Order("id").
		Find(&locked).Error)
	require.Len(t, locked, 2)
	require.NoError(t, mergeTx.Model(&models.ConsentState{}).
		Where("organization_id = ? AND contact_id = ?", organization.ID, source.ID).
		Update("contact_id", canonical.ID).Error)
	require.NoError(t, mergeTx.Unscoped().Model(&models.Contact{}).
		Where("organization_id = ? AND id = ?", organization.ID, source.ID).
		Updates(map[string]any{
			"merged_into_id": canonical.ID,
			"merged_at":      now,
			"deleted_at":     now,
		}).Error)

	type consentResult struct {
		allowed bool
		reason  string
		err     error
	}
	result := make(chan consentResult, 1)
	go func() {
		allowed, reason, err := OutboundConsentAllowed(
			db,
			organization.ID,
			source.ID,
			uuid.New(),
			models.ChannelWhatsApp,
			models.ChannelPreferencePurposeService,
			now,
		)
		result <- consentResult{allowed: allowed, reason: reason, err: err}
	}()

	select {
	case early := <-result:
		require.NoError(t, early.err)
		t.Fatal("outbound consent check completed without waiting for merge row locks")
	case <-time.After(150 * time.Millisecond):
	}

	require.NoError(t, mergeTx.Commit().Error)
	select {
	case checked := <-result:
		require.NoError(t, checked.err)
		assert.False(t, checked.allowed)
		assert.Equal(t, "consent_denied", checked.reason)
	case <-time.After(5 * time.Second):
		t.Fatal("outbound consent check did not resume after merge commit")
	}
}

func createChannelConsentState(
	t *testing.T,
	db *gorm.DB,
	organizationID, contactID uuid.UUID,
	subjectKey string,
	action models.ConsentAction,
	status models.ConsentStatus,
	effectiveAt time.Time,
) {
	t.Helper()
	event := models.ConsentEvent{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organizationID,
		ContactID:      &contactID,
		SubjectType:    "contact",
		SubjectKey:     subjectKey,
		Purpose:        string(models.ChannelPreferencePurposeService),
		Channel:        string(models.ChannelWhatsApp),
		Action:         action,
		Source:         "channel-consent-test",
		Evidence:       models.JSONB{},
		CapturedAt:     effectiveAt,
	}
	require.NoError(t, db.Create(&event).Error)
	state := models.ConsentState{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organizationID,
		ContactID:      &contactID,
		SubjectType:    "contact",
		SubjectKey:     subjectKey,
		Purpose:        string(models.ChannelPreferencePurposeService),
		Channel:        string(models.ChannelWhatsApp),
		Status:         status,
		LatestEventID:  event.ID,
		EffectiveAt:    effectiveAt,
		Metadata:       models.JSONB{},
	}
	require.NoError(t, db.Create(&state).Error)
}
