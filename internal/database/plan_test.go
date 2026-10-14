package database

import (
	"testing"

	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
)

func TestPlanSeedMarkerSourceSemantics(t *testing.T) {
	for _, tc := range []struct {
		raw              string
		manager, product int
	}{
		{`null`, 0, 0}, {`true`, 0, 0}, {`"1"`, 1, 0}, {`" 1 "`, 0, 0}, {`1.9`, 1, 1},
		{`-1`, -1, -1}, {`"bad"`, 0, 0}, {`[]`, 0, 0}, {`{}`, 0, 0}, {`2`, 2, 2},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			v, err := planMarker([]byte(tc.raw))
			require.NoError(t, err)
			require.Equal(t, tc.manager, managerSettingsPolicyVersion(models.JSONB{ManagerSettingsPolicyVersionKey: v}))
			require.Equal(t, tc.product, reReplyCatalogPlanSeedVersion(models.JSONB{reReplyCatalogSeedVersionKey: v}))
		})
	}
	_, err := planMarker([]byte(`not-json`))
	require.ErrorIs(t, err, errPlanSeedUnavailable)
	_, err = planMarker(make([]byte, 65537))
	require.ErrorIs(t, err, errPlanSeedUnavailable)
}

func TestPlanSeedParentThreeValuedOr(t *testing.T) {
	for _, tc := range []struct {
		name     string
		children []PlanSeedObservation
		state    PlanSeedState
		code     string
	}{
		{"clear", []PlanSeedObservation{{State: PlanSeedClear}, {State: PlanSeedClear}}, PlanSeedClear, ""},
		{"unknown", []PlanSeedObservation{{State: PlanSeedClear}, {State: PlanSeedIncomplete, Code: "conflict"}}, PlanSeedIncomplete, "unavailable"},
		{"positive_after_unknown", []PlanSeedObservation{{State: PlanSeedIncomplete, Code: "unavailable"}, {State: PlanSeedDrift}}, PlanSeedDrift, ""},
		{"invalid_is_not_clear", []PlanSeedObservation{{State: PlanSeedInvalid}}, PlanSeedIncomplete, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, code := planSeedParent(tc.children)
			require.Equal(t, tc.state, state)
			require.Equal(t, tc.code, code)
		})
	}
}
