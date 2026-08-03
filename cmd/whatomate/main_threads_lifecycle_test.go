package main

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestIsPublicThreadsLifecyclePathIsNarrow(t *testing.T) {
	t.Parallel()
	organizationID := uuid.MustParse("b0cab0ee-1f9b-4672-ae66-25a4ec82d4c3").String()
	base := "/api/integrations/threads/" + organizationID
	confirmationCode := "THDEL0123456789ABCDEF0123456789ABCDEF"

	for _, path := range []string{
		base + "/deauthorize",
		base + "/data-deletion",
		base + "/data-deletion/status/" + confirmationCode,
	} {
		assert.True(t, isPublicThreadsLifecyclePath(path), path)
	}

	for _, path := range []string{
		"/api/integrations/threads/connect",
		"/api/integrations/threads/test",
		"/api/integrations/threads/credentials",
		base + "/connect",
		base + "/test",
		base + "/credentials",
		base + "/data-deletion/status/THDEL-not-alphanumeric",
		base + "/data-deletion/status/THDEL0123",
		"/api/integrations/threads/not-a-uuid/deauthorize",
	} {
		assert.False(t, isPublicThreadsLifecyclePath(path), path)
	}
}
