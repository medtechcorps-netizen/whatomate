package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestCopilotRunExpiredUsesInclusiveBoundary(t *testing.T) {
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Second)
	future := now.Add(time.Second)

	assert.False(t, copilotRunExpired(nil, now))
	assert.False(t, copilotRunExpired(&models.CopilotRun{}, now))
	assert.True(t, copilotRunExpired(&models.CopilotRun{ExpiresAt: &past}, now))
	assert.True(t, copilotRunExpired(&models.CopilotRun{ExpiresAt: &now}, now))
	assert.False(t, copilotRunExpired(&models.CopilotRun{ExpiresAt: &future}, now))
}

func TestCopilotRetentionPurgeIsTenantScopedAndHardDeletesFeedback(t *testing.T) {
	db := testutil.SetupTestDB(t)
	app := &App{DB: db, Log: testutil.NopLogger()}
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)

	organization := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, organization.ID)
	contact := testutil.CreateTestContact(t, db, organization.ID)
	expired := createCopilotRetentionRun(t, db, organization.ID, user.ID, contact.ID, now.Add(-time.Hour))
	future := createCopilotRetentionRun(t, db, organization.ID, user.ID, contact.ID, now.Add(time.Hour))
	feedback := models.CopilotFeedback{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		RunID:          expired.ID,
		UserID:         user.ID,
		Rating:         models.CopilotFeedbackRatingHelpful,
		Metadata:       models.JSONB{},
		Version:        1,
	}
	require.NoError(t, db.Create(&feedback).Error)

	otherOrganization := testutil.CreateTestOrganization(t, db)
	otherUser := testutil.CreateTestUser(t, db, otherOrganization.ID)
	otherContact := testutil.CreateTestContact(t, db, otherOrganization.ID)
	otherExpired := createCopilotRetentionRun(
		t,
		db,
		otherOrganization.ID,
		otherUser.ID,
		otherContact.ID,
		now.Add(-time.Hour),
	)

	processor := NewCopilotRetentionProcessor(app, time.Hour)
	processor.now = func() time.Time { return now }
	purged, err := processor.purgeOrganization(context.Background(), organization.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), purged)

	assertUnscopedCount(t, db, &models.CopilotRun{}, "id = ?", expired.ID, 0)
	assertUnscopedCount(t, db, &models.CopilotFeedback{}, "id = ?", feedback.ID, 0)
	assertUnscopedCount(t, db, &models.CopilotRun{}, "id = ?", future.ID, 1)
	assertUnscopedCount(t, db, &models.CopilotRun{}, "id = ?", otherExpired.ID, 1)
}

func createCopilotRetentionRun(
	t *testing.T,
	db *gorm.DB,
	organizationID uuid.UUID,
	userID uuid.UUID,
	contactID uuid.UUID,
	expiresAt time.Time,
) *models.CopilotRun {
	t.Helper()
	run := &models.CopilotRun{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organizationID,
		ContactID:      contactID,
		RequestedByID:  userID,
		TaskType:       models.CopilotTaskTypeReply,
		Status:         models.CopilotRunStatusCompleted,
		Model:          "test-model",
		ResultData:     models.JSONB{},
		IdempotencyKey: uuid.NewString(),
		ExpiresAt:      &expiresAt,
		Version:        1,
	}
	require.NoError(t, db.Create(run).Error)
	return run
}

func assertUnscopedCount(
	t *testing.T,
	db *gorm.DB,
	model any,
	query string,
	value any,
	want int64,
) {
	t.Helper()
	var got int64
	require.NoError(t, db.Unscoped().Model(model).Where(query, value).Count(&got).Error)
	assert.Equal(t, want, got)
}
