package testutil

import (
	"encoding/binary"
	"strconv"
	"testing"

	"github.com/google/uuid"
)

// UniqueNumericID returns a collision-resistant numeric identifier suitable
// for test fixtures that exercise provider identity uniqueness constraints.
// Prefix must begin with a non-zero decimal digit.
func UniqueNumericID(t *testing.T, prefix string) string {
	t.Helper()
	id := uuid.New()
	return prefix + strconv.FormatUint(binary.BigEndian.Uint64(id[:8]), 10)
}
