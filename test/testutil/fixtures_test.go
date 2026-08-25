package testutil

import "testing"

func TestNewTestGraphObjectID(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 1000)
	for range 1000 {
		id := NewTestGraphObjectID()
		if len(id) != 32 {
			t.Fatalf("Graph object ID length = %d, want 32", len(id))
		}
		if id[0] == '0' {
			t.Fatal("Graph object ID must not start with zero")
		}
		for _, digit := range id {
			if digit < '0' || digit > '9' {
				t.Fatalf("Graph object ID contains non-decimal character %q", digit)
			}
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate Graph object ID generated: %s", id)
		}
		seen[id] = struct{}{}
	}
}
