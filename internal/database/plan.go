package database

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shridarpatil/whatomate/internal/models"
)

type PlanSeedState uint8

const (
	PlanSeedInvalid PlanSeedState = iota
	PlanSeedClear
	PlanSeedDrift
	PlanSeedIncomplete
)

type PlanSeedObservation struct {
	ID    string
	State PlanSeedState
	Code  string
}

type PlanSeedObservations struct{ Steps [10]PlanSeedObservation }

var errPlanSeedUnavailable = errors.New("seed observation unavailable")
var errPlanSeedConflict = errors.New("seed observation conflict")
var errPlanSeedSession = errors.New("seed observation session unavailable")

func planSeedIDs() [10]string {
	return [10]string{
		"SeedPermissionsAndRoles", "SeedSystemRolesForAllOrgs",
		"SeedSystemRolesForAllOrgs/seedSystemRolesForOrg",
		"SeedSystemRolesForAllOrgs/FixSystemRolePermissions",
		"SeedSystemRolesForAllOrgs/ApplyManagerSettingsPolicyMigration",
		"SeedSystemRolesForAllOrgs/MigrateExistingUserRoles",
		"SeedSystemRolesForAllOrgs/legacy-super-admin-update",
		"EnsurePlatformReseller", "EnsureReReplyProductCatalog", "SeedDefaultWidgets",
	}
}

// ObservePlanSeeds observes exactly the compiled seed semantics in the supplied
// owner transaction. It does not acquire a session, begin/commit/roll back the
// caller's transaction, change roles, or call any seed writer. Main owns the
// single shared deadline, witness and physical session disposal.
func ObservePlanSeeds(ctx context.Context, tx pgx.Tx) (PlanSeedObservations, error) {
	var result PlanSeedObservations
	for i, id := range planSeedIDs() {
		result.Steps[i] = PlanSeedObservation{ID: id, State: PlanSeedIncomplete, Code: "unavailable"}
	}
	// This historical child has no query or desired privilege state. Its
	// removed-write source/revision pair is bound by the compiled PlanSource
	// and generated source checks, never by a live email or privilege lookup.
	result.Steps[6].State, result.Steps[6].Code = PlanSeedClear, ""
	if ctx == nil || tx == nil {
		return result, errPlanSeedSession
	}
	if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil {
		return result, errPlanSeedSession
	}
	if err := planSeedSession(ctx, tx); err != nil {
		return result, errPlanSeedSession
	}
	for _, i := range [...]int{0, 2, 3, 4, 5, 7, 8, 9} {
		if ctx.Err() != nil {
			return result, errPlanSeedSession
		}
		if _, err := tx.Exec(ctx, "SAVEPOINT rereply_plan_seed"); err != nil {
			return result, errPlanSeedSession
		}
		var drift bool
		var err error
		switch i {
		case 0:
			drift, err = planPermissions(ctx, tx)
		case 2:
			drift, err = planSystemRoles(ctx, tx)
		case 3:
			drift, err = planRolePermissions(ctx, tx)
		case 4:
			drift, err = planManagerPolicy(ctx, tx)
		case 5:
			drift, err = planLegacyRoles(ctx, tx)
		case 7:
			drift, err = planPlatformReseller(ctx, tx)
		case 8:
			drift, err = planProductCatalog(ctx, tx)
		case 9:
			drift, err = planWidgets(ctx, tx)
		}
		if err != nil {
			if _, recoveryErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT rereply_plan_seed"); recoveryErr != nil {
				return result, errPlanSeedSession
			}
			if errors.Is(err, errPlanSeedConflict) {
				result.Steps[i].Code = "conflict"
			}
		} else {
			result.Steps[i].State, result.Steps[i].Code = PlanSeedClear, ""
			if drift {
				result.Steps[i].State = PlanSeedDrift
			}
		}
		if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT rereply_plan_seed"); err != nil {
			return result, errPlanSeedSession
		}
	}
	result.Steps[1].State, result.Steps[1].Code = planSeedParent(result.Steps[2:7])
	if err := planSeedSession(ctx, tx); err != nil {
		return result, errPlanSeedSession
	}
	return result, nil
}

func planSeedParent(children []PlanSeedObservation) (PlanSeedState, string) {
	incomplete := false
	for _, child := range children {
		if child.State == PlanSeedDrift {
			return PlanSeedDrift, ""
		}
		if child.State != PlanSeedClear {
			incomplete = true
		}
	}
	if incomplete {
		return PlanSeedIncomplete, "unavailable"
	}
	return PlanSeedClear, ""
}

func planSeedSession(ctx context.Context, tx pgx.Tx) error {
	var eligible bool
	var statement, lock, idle string
	err := tx.QueryRow(ctx, `SELECT current_user = session_user AND NOT r.rolsuper AND NOT r.rolbypassrls
 AND NOT r.rolcreaterole AND NOT r.rolreplication
 AND pg_catalog.current_setting('server_version_num')::integer / 10000 = 17
 AND NOT pg_catalog.pg_is_in_recovery()
 AND pg_catalog.current_setting('transaction_read_only') = 'on'
 AND pg_catalog.current_setting('transaction_isolation') = 'repeatable read'
 AND pg_catalog.current_setting('search_path') = 'pg_catalog'
 AND pg_catalog.current_setting('server_encoding') = 'UTF8'
 AND pg_catalog.current_setting('session_replication_role') = 'origin'
 AND (SELECT a.usename = session_user FROM pg_catalog.pg_stat_activity a WHERE a.pid = pg_catalog.pg_backend_pid()),
 pg_catalog.current_setting('statement_timeout'),pg_catalog.current_setting('lock_timeout'),
 pg_catalog.current_setting('idle_in_transaction_session_timeout')
 FROM pg_catalog.pg_roles r WHERE r.rolname=current_user`).Scan(&eligible, &statement, &lock, &idle)
	if err != nil || !eligible {
		return errPlanSeedSession
	}
	for _, v := range []struct {
		value   string
		maximum time.Duration
	}{{statement, 3 * time.Second}, {lock, time.Second}, {idle, 5 * time.Second}} {
		d, err := time.ParseDuration(v.value)
		if err != nil || d <= 0 || d > v.maximum {
			return errPlanSeedSession
		}
	}
	return ctx.Err()
}

const planOrdinaryOrganization = `o.deleted_at IS NULL
 AND NOT pg_catalog.jsonb_exists(COALESCE(o.settings,'{}'::jsonb),$1)
 AND NOT pg_catalog.jsonb_exists(COALESCE(o.settings,'{}'::jsonb),$2)
 AND NOT pg_catalog.jsonb_exists(COALESCE(o.settings,'{}'::jsonb),$3)`

func planOrganizationArgs() []any {
	return []any{PlatformCompliancePurposeMarkerKey, PlatformComplianceInstagramMarkerKey, PlatformComplianceThreadsMarkerKey}
}

func planPermissions(ctx context.Context, tx pgx.Tx) (bool, error) {
	if err := planSeedTables(ctx, tx, "permissions"); err != nil {
		return false, err
	}
	type pair struct {
		Resource string `json:"resource"`
		Action   string `json:"action"`
	}
	pairs := make([]pair, 0)
	for _, p := range models.DefaultPermissions() {
		pairs = append(pairs, pair{p.Resource, p.Action})
	}
	raw, err := json.Marshal(pairs)
	if err != nil {
		return false, errPlanSeedUnavailable
	}
	var drift, conflict bool
	err = tx.QueryRow(ctx, `WITH expected AS (SELECT * FROM pg_catalog.jsonb_to_recordset($1::jsonb) AS e(resource text,action text)),
 missing AS (SELECT e.* FROM expected e WHERE NOT EXISTS(SELECT 1 FROM public.permissions p
 WHERE p.deleted_at IS NULL AND p.resource=e.resource AND p.action=e.action))
 SELECT EXISTS(SELECT 1 FROM missing),EXISTS(SELECT 1 FROM missing e JOIN public.permissions p
 ON p.resource=e.resource AND p.action=e.action WHERE p.deleted_at IS NOT NULL)`, string(raw)).Scan(&drift, &conflict)
	// The source writer's scoped lookup followed by Create cannot replace a
	// deleted row protected by the unconditional resource/action unique index.
	if err == nil && conflict {
		return false, errPlanSeedConflict
	}
	return drift, err
}

func planSystemRoles(ctx context.Context, tx pgx.Tx) (bool, error) {
	if err := planSeedTables(ctx, tx, "organizations", "custom_roles"); err != nil {
		return false, err
	}
	var drift bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.organizations o WHERE `+planOrdinaryOrganization+`
 AND NOT EXISTS(SELECT 1 FROM public.custom_roles r WHERE r.organization_id=o.id AND r.deleted_at IS NULL AND r.is_system))`, planOrganizationArgs()...).Scan(&drift)
	return drift, err
}

func planRolePermissions(ctx context.Context, tx pgx.Tx) (bool, error) {
	if err := planSeedTables(ctx, tx, "permissions", "custom_roles", "role_permissions"); err != nil {
		return false, err
	}
	type expected struct {
		Role     string `json:"role"`
		Resource string `json:"resource"`
		Action   string `json:"action"`
	}
	var entries []expected
	roles := models.SystemRolePermissions()
	names := make([]string, 0, len(roles))
	for name := range roles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, key := range roles[name] {
			if key == bookingSettingsDeletePermissionKey {
				continue
			}
			resource, action, ok := strings.Cut(key, ":")
			if !ok {
				return false, errPlanSeedUnavailable
			}
			entries = append(entries, expected{name, resource, action})
		}
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return false, errPlanSeedUnavailable
	}
	// GORM scans nullable timestamps into time.Time zero. Preserve !Before,
	// including equality and a NULL role timestamp, instead of SQL NULL truth.
	var drift, ambiguous bool
	err = tx.QueryRow(ctx, `WITH expected AS (SELECT * FROM pg_catalog.jsonb_to_recordset($1::jsonb) AS e(role text,resource text,action text))
 SELECT EXISTS(SELECT 1 FROM public.custom_roles r JOIN expected e ON e.role=r.name
 JOIN public.permissions p ON p.resource=e.resource AND p.action=e.action AND p.deleted_at IS NULL
 WHERE r.deleted_at IS NULL AND r.is_system
 AND NOT EXISTS(SELECT 1 FROM public.role_permissions rp WHERE rp.custom_role_id=r.id AND rp.permission_id=p.id)
 AND (COALESCE(p.created_at,'0001-01-01 00:00:00+00'::timestamptz) >= COALESCE(r.updated_at,'0001-01-01 00:00:00+00'::timestamptz) OR NOT EXISTS(SELECT 1 FROM public.role_permissions rp
 JOIN public.permissions live ON live.id=rp.permission_id AND live.deleted_at IS NULL WHERE rp.custom_role_id=r.id))),
 EXISTS(SELECT 1 FROM public.permissions p JOIN expected e ON e.resource=p.resource AND e.action=p.action
 WHERE p.deleted_at IS NULL GROUP BY p.resource,p.action HAVING pg_catalog.count(DISTINCT p.id)>1)`, string(raw)).Scan(&drift, &ambiguous)
	if err == nil && ambiguous {
		return false, errPlanSeedConflict
	}
	return drift, err
}

func planManagerPolicy(ctx context.Context, tx pgx.Tx) (bool, error) {
	if err := planSeedTables(ctx, tx, "organizations"); err != nil {
		return false, err
	}
	args := append(planOrganizationArgs(), ManagerSettingsPolicyVersionKey)
	rows, err := tx.Query(ctx, `SELECT o.settings -> $4 FROM public.organizations o WHERE `+planOrdinaryOrganization, args...)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	drift := false
	for rows.Next() {
		if ctx.Err() != nil {
			return false, errPlanSeedUnavailable
		}
		var raw []byte
		if rows.Scan(&raw) != nil {
			return false, errPlanSeedUnavailable
		}
		v, err := planMarker(raw)
		if err != nil {
			return false, err
		}
		if managerSettingsPolicyVersion(models.JSONB{ManagerSettingsPolicyVersionKey: v}) < ManagerSettingsPolicyVersion {
			drift = true
		}
	}
	return drift, rows.Err()
}

func planMarker(raw []byte) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > 65536 {
		return nil, errPlanSeedUnavailable
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil, errPlanSeedUnavailable
	}
	return value, nil
}

func planLegacyRoles(ctx context.Context, tx pgx.Tx) (bool, error) {
	if err := planSeedTables(ctx, tx, "users", "custom_roles"); err != nil {
		return false, err
	}
	var present, foreign bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_attribute a
 JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND c.relname='users' AND a.attname='role' AND a.attnum>0 AND NOT a.attisdropped),
 EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='users' AND column_name='role' AND table_schema<>'public')`).Scan(&present, &foreign)
	if err != nil {
		return false, err
	}
	if !present {
		if foreign {
			return false, errPlanSeedUnavailable
		}
		return false, nil
	}
	var drift, ambiguous bool
	err = tx.QueryRow(ctx, `WITH candidates AS (
 SELECT (SELECT pg_catalog.count(*) FROM public.custom_roles r WHERE r.organization_id=u.organization_id
 AND r.name=u.role AND r.is_system AND r.deleted_at IS NULL) AS matches
 FROM public.users u WHERE u.role_id IS NULL AND u.role IS NOT NULL AND u.role<>'')
 SELECT EXISTS(SELECT 1 FROM candidates WHERE matches=1),EXISTS(SELECT 1 FROM candidates WHERE matches>1)`).Scan(&drift, &ambiguous)
	if err == nil && ambiguous {
		return false, errPlanSeedConflict
	}
	return drift, err
}

func planPlatformReseller(ctx context.Context, tx pgx.Tx) (bool, error) {
	if err := planSeedTables(ctx, tx, "resellers", "reseller_members", "organizations", "users"); err != nil {
		return false, err
	}
	var drift, ambiguous bool
	err := tx.QueryRow(ctx, `WITH platform AS (SELECT id,deleted_at,status FROM public.resellers WHERE slug=$1)
 SELECT NOT EXISTS(SELECT 1 FROM platform) OR EXISTS(SELECT 1 FROM platform WHERE deleted_at IS NOT NULL OR status IS DISTINCT FROM $2)
 OR EXISTS(SELECT 1 FROM public.organizations WHERE deleted_at IS NULL AND reseller_id IS NULL)
 OR EXISTS(SELECT 1 FROM public.users u CROSS JOIN platform r WHERE u.deleted_at IS NULL AND u.is_super_admin AND u.is_active
 AND (NOT EXISTS(SELECT 1 FROM public.reseller_members m WHERE m.reseller_id=r.id AND m.user_id=u.id)
 OR EXISTS(SELECT 1 FROM public.reseller_members m WHERE m.reseller_id=r.id AND m.user_id=u.id
 AND (m.deleted_at IS NOT NULL OR m.role IS DISTINCT FROM $3 OR m.is_active IS DISTINCT FROM true)))),
 (SELECT pg_catalog.count(*) FROM platform)>1 OR EXISTS(SELECT 1 FROM public.reseller_members m JOIN platform r ON r.id=m.reseller_id
 JOIN public.users u ON u.id=m.user_id WHERE u.deleted_at IS NULL AND u.is_super_admin AND u.is_active
 GROUP BY m.reseller_id,m.user_id HAVING pg_catalog.count(*)>1)`, PlatformResellerSlug, models.ResellerStatusActive, models.ResellerRoleOwner).Scan(&drift, &ambiguous)
	if err == nil && ambiguous {
		return false, errPlanSeedConflict
	}
	return drift, err
}

func planWidgets(ctx context.Context, tx pgx.Tx) (bool, error) {
	if err := planSeedTables(ctx, tx, "organizations", "users", "widgets"); err != nil {
		return false, err
	}
	var drift bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.users WHERE deleted_at IS NULL AND is_super_admin)
 AND EXISTS(SELECT 1 FROM public.organizations o WHERE `+planOrdinaryOrganization+`
 AND NOT EXISTS(SELECT 1 FROM public.widgets w WHERE w.organization_id=o.id AND w.deleted_at IS NULL))`, planOrganizationArgs()...).Scan(&drift)
	return drift, err
}

func planProductCatalog(ctx context.Context, tx pgx.Tx) (bool, error) {
	if err := planSeedTables(ctx, tx, "plans", "plan_prices", "plan_entitlements"); err != nil {
		return false, err
	}
	drift := false
	for _, seed := range reReplyProductCatalog {
		if ctx.Err() != nil {
			return false, errPlanSeedUnavailable
		}
		rows, err := tx.Query(ctx, `SELECT id::text,deleted_at IS NOT NULL,metadata -> $3 FROM public.plans WHERE scope_key=$1 AND code=$2 LIMIT 2`, reReplyCatalogScopeKey, seed.Code, reReplyCatalogSeedVersionKey)
		if err != nil {
			return false, err
		}
		var id string
		var deleted bool
		var marker []byte
		count := 0
		for rows.Next() {
			count++
			if err = rows.Scan(&id, &deleted, &marker); err != nil {
				break
			}
		}
		rows.Close()
		if err != nil {
			return false, err
		}
		if rows.Err() != nil {
			return false, rows.Err()
		}
		if count > 1 {
			return false, errPlanSeedConflict
		}
		if count == 0 {
			drift = true
			continue
		}
		if deleted {
			continue
		}
		value, err := planMarker(marker)
		if err != nil {
			return false, err
		}
		backfill := reReplyCatalogPlanSeedVersion(models.JSONB{reReplyCatalogSeedVersionKey: value}) < reReplyCatalogSeedVersion
		if backfill {
			drift = true
		}
		var prices int64
		var conflict bool
		err = tx.QueryRow(ctx, `SELECT pg_catalog.count(*),COALESCE(pg_catalog.bool_or(deleted_at IS NULL AND (
 provider IS DISTINCT FROM $3 OR currency IS DISTINCT FROM 'MYR' OR unit_amount_minor IS DISTINCT FROM $4
 OR setup_amount_minor IS DISTINCT FROM 0 OR interval IS DISTINCT FROM $5 OR interval_count IS DISTINCT FROM 1
 OR tax_behavior IS DISTINCT FROM 'exclusive' OR effective_from IS NOT NULL OR effective_until IS NOT NULL)),false)
 FROM public.plan_prices WHERE plan_id=$1::uuid AND code=$2`, id, seed.PriceCode, string(models.BillingProviderManual), seed.UnitAmountMinor, string(models.BillingIntervalMonth)).Scan(&prices, &conflict)
		if err != nil {
			return false, err
		}
		if prices > 1 || conflict {
			return false, errPlanSeedConflict
		}
		if prices == 0 {
			drift = true
		}
		if backfill {
			// The marker write already establishes drift, including required
			// entitlement normalization and the exact legacy placeholder metadata
			// retirement. A deleted required entitlement instead makes it conflict.
			keys := make([]string, 0, len(seed.EntitlementState))
			for key := range seed.EntitlementState {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.plan_entitlements WHERE plan_id=$1::uuid AND key=ANY($2::text[]) AND deleted_at IS NOT NULL)
 OR EXISTS(SELECT 1 FROM public.plan_entitlements WHERE plan_id=$1::uuid AND key=ANY($2::text[]) GROUP BY key HAVING pg_catalog.count(*)>1)`, id, keys).Scan(&conflict)
			if err != nil {
				return false, err
			}
			if conflict {
				return false, errPlanSeedConflict
			}
		}
	}
	return drift, nil
}
