package worker

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/threadsreview"
	"github.com/stretchr/testify/require"
)

func approvedThreadsWorkerTestConfig(t *testing.T, input models.JSONB, appID string) models.JSONB {
	t.Helper()
	approved, ok := threadsreview.RecordApproval(
		input,
		appID,
		"synthetic Meta App Review evidence for worker tests",
		uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"),
		time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
	)
	require.True(t, ok)
	return approved
}
