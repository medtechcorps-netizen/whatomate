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

func TestPlanSeedHeldOwnerCanonicalMarker(t *testing.T) {
	owner := PlanSeedHeldOwner{PID: 1, DatabaseOID: 2, DatabaseName: "synthetic_db", RoleOID: 3, RoleName: "synthetic_owner"}
	for _, marker := range []string{
		"rereply-plan-lock-00000000-0000-4000-8000-000000000000",
		"rereply-plan-lock-01234567-89ab-4cde-9f01-23456789abcd",
		"rereply-plan-lock-ffffffff-ffff-4fff-afff-ffffffffffff",
		"rereply-plan-lock-ffffffff-ffff-4fff-bfff-ffffffffffff",
	} {
		owner.Marker = marker
		require.True(t, validPlanSeedHeldOwner(owner), "canonical UUIDv4 %q", marker)
	}
	for _, marker := range []string{
		"", "rereply-plan-lock-", "rereply-plan-lock-00000000-0000-0000-0000-000000000000",
		"rereply-plan-lock-01234567-89ab-1cde-8f01-23456789abcd",
		"rereply-plan-lock-01234567-89ab-4cde-7f01-23456789abcd",
		"rereply-plan-lock-01234567-89ab-4cde-cf01-23456789abcd",
		"rereply-plan-lock-01234567-89ab-4cde-df01-23456789abcd",
		"rereply-plan-lock-01234567-89ab-4cde-ef01-23456789abcd",
		"rereply-plan-lock-01234567-89ab-4cde-ff01-23456789abcd",
		"rereply-plan-lock-01234567-89AB-4cde-8f01-23456789abcd",
		"rereply-plan-lock-01234567-89ab-4cde-8f01-23456789abcg",
		"rereply-plan-lock-01234567089ab-4cde-8f01-23456789abcd",
		"rereply-plan-lock-01234567-89ab04cde-8f01-23456789abcd",
		"rereply-plan-lock-01234567-89ab-4cde08f01-23456789abcd",
		"rereply-plan-lock-01234567-89ab-4cde-8f01023456789abcd",
		"rereply-plan-lock-01234567-89ab-4cde-8f01-23456789abc",
		"rereply-plan-lock-01234567-89ab-4cde-8f01-23456789abcde",
		"Rereply-plan-lock-01234567-89ab-4cde-8f01-23456789abcd",
		"rereply-plan-lock-{01234567-89ab-4cde-8f01-23456789abcd}",
		"rereply-plan-lock-0123456789ab4cde8f0123456789abcd",
		"rereply-plan-lock-01234567-89ab-4cde-8f01-23456789abcd\n",
	} {
		owner.Marker = marker
		require.False(t, validPlanSeedHeldOwner(owner), "noncanonical marker %q", marker)
	}
	owner.Marker = "rereply-plan-lock-01234567-89ab-4cde-8f01-23456789abcd"
	wrong := []PlanSeedHeldOwner{owner, owner, owner, owner, owner, owner, owner, owner}
	wrong[0].PID = 0
	wrong[1].PID = 1 << 32
	wrong[2].DatabaseOID = 0
	wrong[3].DatabaseOID = 1 << 32
	wrong[4].RoleOID = 0
	wrong[5].RoleOID = 1 << 32
	wrong[6].DatabaseName = ""
	wrong[7].RoleName = ""
	for i, candidate := range wrong {
		require.False(t, validPlanSeedHeldOwner(candidate), "identity boundary %d", i)
	}
}
