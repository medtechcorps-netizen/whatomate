package threadsreview

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProductionApprovedRequiresServerEvidenceBoundToExactApp(t *testing.T) {
	const appID = "123456789012345"
	actor := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	approved, ok := RecordApproval(
		models.JSONB{"app_id": appID, StatusKey: "pending"},
		appID,
		"synthetic approval evidence",
		actor,
		time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
	)
	require.True(t, ok)
	assert.True(t, ProductionApproved(approved))
	assert.False(t, ProductionApproved(models.JSONB{"app_id": appID, StatusKey: ApprovedStatus}))
	paddedStatus := clone(approved)
	paddedStatus[StatusKey] = " approved "
	assert.False(t, ProductionApproved(paddedStatus))

	wrongApp := clone(approved)
	wrongApp["app_id"] = "987654321098765"
	assert.False(t, ProductionApproved(wrongApp))

	malformed := clone(approved)
	malformed[ApprovalEvidenceKey] = models.JSONB{"app_id": appID}
	assert.False(t, ProductionApproved(malformed))
}

func TestAccessModeDevelopmentTestingRequiresExactNonProductionTuple(t *testing.T) {
	organizationID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	const appID = "123456789012345"
	const profileID = "987654321098765"
	cfg := &config.Config{
		App: config.AppConfig{Environment: "staging"},
		ThreadsAppReview: config.ThreadsAppReviewConfig{
			DevelopmentTestingEnabled: true,
			DevelopmentOrganizationID: organizationID.String(),
			DevelopmentAppID:          appID,
			DevelopmentProfileID:      profileID,
		},
	}
	pending := models.JSONB{"app_id": appID, StatusKey: "pending"}
	assert.Equal(t, ModeDevelopmentTesting, AccessMode(cfg, organizationID, pending, appID, ""))
	assert.Equal(t, ModeDevelopmentTesting, AccessMode(cfg, organizationID, pending, appID, profileID))
	assert.Equal(t, ModeBlocked, AccessMode(cfg, uuid.New(), pending, appID, profileID))
	assert.Equal(t, ModeBlocked, AccessMode(cfg, organizationID, pending, "111", profileID))
	assert.Equal(t, ModeBlocked, AccessMode(cfg, organizationID, pending, appID, "222"))
	assert.Equal(t, ModeBlocked, AccessMode(cfg, organizationID, models.JSONB{"app_id": appID, StatusKey: "rejected"}, appID, profileID))

	cfg.App.Environment = "production"
	assert.Equal(t, ModeBlocked, AccessMode(cfg, organizationID, pending, appID, profileID))
}
