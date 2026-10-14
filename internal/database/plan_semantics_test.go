package database_test

import (
	"context"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/stretchr/testify/require"
)

// The fixture and the owned transaction are the shared real PG17 helpers.
// This observer-only oracle checks the borrowed transaction and every persisted
// seed row; it never invokes a writer to normalize the state under observation.
func planObserveFixture(t *testing.T, f catalogFixture) database.PlanSeedObservations {
	t.Helper()
	before := catalogPlanRows(t, f)
	start := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(15*time.Second))
	defer cancel()
	ownerURL := catalogRoleURL(catalogTemplate.base, f.name, catalogTemplate.owner, catalogTemplate.ownerPassword)
	tx, done := catalogPlanTransaction(t, ctx, start.Add(17*time.Second), ownerURL.String())
	got, err := database.ObservePlanSeeds(ctx, tx)
	require.NoError(t, err)
	catalogPlanBorrowStillOpen(t, ctx, tx)
	done()
	require.NoError(t, ctx.Err())
	require.Equal(t, before, catalogPlanRows(t, f))
	require.Equal(t, database.PlanSeedClear, got.Steps[6].State)
	require.Empty(t, got.Steps[6].Code)
	return got
}

func TestTenantRLS_PlanSeedSemanticBoundaries(t *testing.T) {
	// One malformed-marker fixture temporarily disables owned user triggers only
	// during setup, then restores them before observation. The observer still has
	// to exclude key presence regardless of the value; it cannot bless the marker.
	cases := []struct {
		name  string
		setup []string
		index int
		state database.PlanSeedState
		code  string
	}{
		{"soft_deleted_permission", []string{"UPDATE public.permissions SET deleted_at=NOW() WHERE resource='contacts' AND action='read'"}, 0, database.PlanSeedIncomplete, "conflict"},
		{"one_system_role_skips_population", []string{"UPDATE public.custom_roles SET is_system=false WHERE name<>'admin'"}, 2, database.PlanSeedClear, ""},
		{"compliance_marker_presence_false", []string{"UPDATE public.custom_roles SET is_system=false", "ALTER TABLE public.organizations DISABLE TRIGGER USER", "UPDATE public.organizations SET settings=settings || '{\"platform_compliance_tenant\":false}'::jsonb", "ALTER TABLE public.organizations ENABLE TRIGGER USER"}, 2, database.PlanSeedClear, ""},
		{"manager_string_marker_preserved", []string{"UPDATE public.organizations SET settings=jsonb_set(settings,'{system_manager_settings_policy_version}','\"1\"'::jsonb)"}, 4, database.PlanSeedClear, ""},
		{"manager_marker_without_manager", []string{"UPDATE public.custom_roles SET name='synthetic-manager' WHERE name='manager'", "UPDATE public.organizations SET settings=settings-'system_manager_settings_policy_version'"}, 4, database.PlanSeedDrift, ""},
		{"raw_soft_deleted_legacy_user", []string{"ALTER TABLE public.users ADD COLUMN role text", "UPDATE public.users SET role_id=NULL,role='admin',deleted_at=NOW() WHERE email='catalog@example.invalid'"}, 5, database.PlanSeedDrift, ""},
		{"unknown_legacy_name_is_skipped", []string{"ALTER TABLE public.users ADD COLUMN role text", "UPDATE public.users SET role_id=NULL,role='synthetic-unknown' WHERE email='catalog@example.invalid'"}, 5, database.PlanSeedClear, ""},
		{"foreign_only_legacy_column", []string{"CREATE SCHEMA synthetic_legacy", "CREATE TABLE synthetic_legacy.users(role text)"}, 5, database.PlanSeedIncomplete, "unavailable"},
		{"null_membership_active_needs_restoration", []string{"UPDATE public.reseller_members SET is_active=NULL WHERE user_id=(SELECT id FROM public.users WHERE email='catalog@example.invalid')"}, 7, database.PlanSeedDrift, ""},
		{"deleted_product_is_intentionally_skipped", []string{"UPDATE public.plans SET deleted_at=NOW() WHERE code='rereply-starter'", "UPDATE public.plan_prices SET unit_amount_minor=1 WHERE plan_id=(SELECT id FROM public.plans WHERE code='rereply-starter')"}, 8, database.PlanSeedClear, ""},
		{"deleted_price_is_intentionally_skipped", []string{"UPDATE public.plan_prices SET deleted_at=NOW(),unit_amount_minor=1 WHERE plan_id=(SELECT id FROM public.plans WHERE code='rereply-starter')"}, 8, database.PlanSeedClear, ""},
		{"product_string_marker_requires_backfill", []string{"UPDATE public.plans SET metadata=jsonb_set(metadata,'{rereply_catalog_seed_version}','\"1\"'::jsonb) WHERE code='rereply-starter'"}, 8, database.PlanSeedDrift, ""},
		{"immutable_price_is_conflict", []string{"UPDATE public.plan_prices SET unit_amount_minor=1 WHERE plan_id=(SELECT id FROM public.plans WHERE code='rereply-starter')"}, 8, database.PlanSeedIncomplete, "conflict"},
		{"deleted_required_entitlement_is_conflict", []string{"UPDATE public.plans SET metadata=jsonb_set(metadata,'{rereply_catalog_seed_version}','0'::jsonb) WHERE code='rereply-starter'", "UPDATE public.plan_entitlements SET deleted_at=NOW() WHERE plan_id=(SELECT id FROM public.plans WHERE code='rereply-starter')"}, 8, database.PlanSeedIncomplete, "conflict"},
		{"operator_entitlement_edits_after_marker_are_preserved", []string{"UPDATE public.plan_entitlements SET deleted_at=NOW() WHERE plan_id=(SELECT id FROM public.plans WHERE code='rereply-starter')"}, 8, database.PlanSeedClear, ""},
		{"inactive_super_admin_still_enables_widgets", []string{"UPDATE public.users SET is_active=false WHERE is_super_admin", "UPDATE public.widgets SET deleted_at=NOW()"}, 9, database.PlanSeedDrift, ""},
		{"no_live_super_admin_skips_widgets", []string{"UPDATE public.users SET deleted_at=NOW() WHERE is_super_admin", "UPDATE public.widgets SET deleted_at=NOW()"}, 9, database.PlanSeedClear, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := catalogClone(t)
			for _, sql := range tc.setup {
				require.NoError(t, f.owner.Exec(sql).Error)
			}
			got := planObserveFixture(t, f)
			require.Equal(t, tc.state, got.Steps[tc.index].State, got.Steps[tc.index].ID)
			require.Equal(t, tc.code, got.Steps[tc.index].Code)
		})
	}
}

func TestTenantRLS_PlanRolePermissionTimestampBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, created string
		nullUpdated   bool
		want          database.PlanSeedState
	}{
		{"older_removed_grant", "2025-01-01T00:00:00Z", false, database.PlanSeedClear},
		{"equal_timestamp_missing_grant", "2025-01-02T00:00:00Z", false, database.PlanSeedDrift},
		{"null_role_timestamp_loads_zero", "2025-01-01T00:00:00Z", true, database.PlanSeedDrift},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := catalogClone(t)
			var linked int64
			require.NoError(t, f.owner.Raw("SELECT count(*) FROM public.role_permissions rp JOIN public.custom_roles r ON r.id=rp.custom_role_id WHERE r.name='admin'").Scan(&linked).Error)
			require.Greater(t, linked, int64(1))
			require.NoError(t, f.owner.Exec("DELETE FROM public.role_permissions WHERE custom_role_id IN(SELECT id FROM public.custom_roles WHERE name='admin') AND permission_id IN(SELECT id FROM public.permissions WHERE resource='contacts' AND action='read')").Error)
			require.NoError(t, f.owner.Exec("UPDATE public.custom_roles SET updated_at='2025-01-02T00:00:00Z' WHERE name='admin'").Error)
			require.NoError(t, f.owner.Exec("UPDATE public.permissions SET created_at=? WHERE resource='contacts' AND action='read'", tc.created).Error)
			if tc.nullUpdated {
				require.NoError(t, f.owner.Exec("UPDATE public.custom_roles SET updated_at=NULL WHERE name='admin'").Error)
			}
			got := planObserveFixture(t, f)
			require.Equal(t, tc.want, got.Steps[3].State)
		})
	}
	t.Run("explicit_booking_delete_absence_is_not_repaired", func(t *testing.T) {
		f := catalogClone(t)
		require.NoError(t, f.owner.Exec("DELETE FROM public.role_permissions WHERE permission_id IN(SELECT id FROM public.permissions WHERE resource='booking.settings' AND action='delete')").Error)
		require.NoError(t, f.owner.Exec("UPDATE public.permissions SET created_at='2099-01-01T00:00:00Z' WHERE resource='booking.settings' AND action='delete'").Error)
		got := planObserveFixture(t, f)
		require.Equal(t, database.PlanSeedClear, got.Steps[3].State)
	})
}

func TestTenantRLS_PlanSeedVisibilityAndSavepointRecovery(t *testing.T) {
	t.Run("global_rls_cannot_fabricate_empty_clear", func(t *testing.T) {
		f := catalogClone(t)
		require.NoError(t, f.owner.Exec("ALTER TABLE public.permissions ENABLE ROW LEVEL SECURITY").Error)
		require.NoError(t, f.owner.Exec("ALTER TABLE public.permissions FORCE ROW LEVEL SECURITY").Error)
		got := planObserveFixture(t, f)
		require.Equal(t, database.PlanSeedIncomplete, got.Steps[0].State)
		require.Equal(t, database.PlanSeedIncomplete, got.Steps[3].State)
		require.Equal(t, database.PlanSeedClear, got.Steps[8].State, "independent later observations still run")
	})
	t.Run("tenant_visibility_policy_is_required", func(t *testing.T) {
		f := catalogClone(t)
		require.NoError(t, f.owner.Exec("DROP POLICY rereply_migration_access ON public.widgets").Error)
		got := planObserveFixture(t, f)
		require.Equal(t, database.PlanSeedIncomplete, got.Steps[9].State)
	})
	t.Run("query_error_is_rolled_back_before_next_seed", func(t *testing.T) {
		f := catalogClone(t)
		require.NoError(t, f.owner.Exec("ALTER TABLE public.permissions RENAME COLUMN resource TO unavailable_resource").Error)
		got := planObserveFixture(t, f)
		require.Equal(t, database.PlanSeedIncomplete, got.Steps[0].State)
		require.Equal(t, database.PlanSeedIncomplete, got.Steps[3].State)
		require.Equal(t, database.PlanSeedClear, got.Steps[4].State)
		require.Equal(t, database.PlanSeedClear, got.Steps[8].State)
	})
	t.Run("runtime_role_is_not_owner_authority", func(t *testing.T) {
		f := catalogClone(t)
		start := time.Now()
		ctx, cancel := context.WithDeadline(context.Background(), start.Add(15*time.Second))
		defer cancel()
		tx, done := catalogPlanTransaction(t, ctx, start.Add(17*time.Second), f.runtimeDSN)
		got, err := database.ObservePlanSeeds(ctx, tx)
		require.NoError(t, err)
		for _, i := range []int{0, 2, 3, 4, 5, 7, 8, 9} {
			require.Equal(t, database.PlanSeedIncomplete, got.Steps[i].State)
		}
		catalogPlanBorrowStillOpen(t, ctx, tx)
		done()
	})
}
