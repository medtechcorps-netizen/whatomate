package handlers

import (
	"errors"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/threadsreview"
)

const threadsAppReviewApprovedStatus = threadsreview.ApprovedStatus

var errThreadsAppReviewApprovalRequired = errors.New("threads Meta App Review approval is required")

// threadsAppReviewApproved is the single fail-closed decision used by the
// integration settings and OAuth lifecycle. Missing, tenant-written, or
// malformed evidence never authorizes Threads.
func threadsAppReviewApproved(config models.JSONB) bool {
	return threadsreview.ProductionApproved(config)
}

func (a *App) threadsAppReviewAccessMode(
	organizationID uuid.UUID,
	config models.JSONB,
	profileID string,
) string {
	return threadsreview.AccessMode(
		a.Config,
		organizationID,
		config,
		stringJSONValue(config, "app_id"),
		profileID,
	)
}

func (a *App) requireThreadsAppReviewAccess(
	organizationID uuid.UUID,
	config models.JSONB,
	profileID string,
) (string, error) {
	mode := a.threadsAppReviewAccessMode(organizationID, config, profileID)
	if mode == threadsreview.ModeBlocked {
		return mode, errThreadsAppReviewApprovalRequired
	}
	return mode, nil
}
