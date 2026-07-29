package main

import (
	"errors"
	"strings"
	"testing"
)

func TestSafeStartupReasonNeverExposesUnderlyingError(t *testing.T) {
	const sensitive = "redis://:super-secret@example.test/0"
	reason := safeStartupReason(errors.New(sensitive))
	if reason != "startup_or_runtime_failure" {
		t.Fatalf("unexpected safe reason %q", reason)
	}
	if strings.Contains(reason, "super-secret") || strings.Contains(reason, "example.test") {
		t.Fatalf("startup reason exposed source error: %q", reason)
	}
}
