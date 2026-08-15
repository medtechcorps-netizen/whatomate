package handlers

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/threadsreview"
	"github.com/stretchr/testify/require"
)

var threadsTestApprovalActor = uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")

func approvedThreadsTestConfig(t *testing.T, input models.JSONB, appID string) models.JSONB {
	t.Helper()
	approved, ok := threadsreview.RecordApproval(
		input,
		appID,
		"synthetic Meta App Review evidence for automated tests",
		threadsTestApprovalActor,
		time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
	)
	require.True(t, ok)
	return approved
}
