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
	return observePlanSeeds(ctx, tx, nil)
}

func observePlanSeeds(ctx context.Context, tx pgx.Tx, owner *PlanSeedHeldOwner) (PlanSeedObservations, error) {
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
	if err := planSeedSession(ctx, tx, owner); err != nil {
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
	if err := planSeedSession(ctx, tx, owner); err != nil {
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

func planSeedSession(ctx context.Context, tx pgx.Tx, owner *PlanSeedHeldOwner) error {
	nativeOwner := false
	if owner != nil {
		if err := planSeedHeldOwnerSession(ctx, tx, *owner); err != nil {
			return errPlanSeedSession
		}
		nativeOwner = true
	}
	var eligible bool
	var statement, lock, idle string
	err := tx.QueryRow(ctx, `SELECT current_user = session_user AND NOT r.rolsuper
 AND ((NOT r.rolbypassrls AND NOT r.rolcreaterole AND NOT r.rolreplication) OR CAST($1 AS boolean))
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
 FROM pg_catalog.pg_roles r WHERE r.rolname=current_user`, nativeOwner).Scan(&eligible, &statement, &lock, &idle)
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

// PlanSeedHeldOwner is the native identity already observed on the migration
// coordinator's separate, still-held connection. It is an expectation, not an
// authority flag: the observer independently matches it to PostgreSQL before
// and after reading seeds. It never selects a connection or changes a role.
type PlanSeedHeldOwner struct {
	PID          int64
	DatabaseOID  int64
	DatabaseName string
	RoleOID      int64
	RoleName     string
	Marker       string
}

func validPlanSeedHeldOwner(owner PlanSeedHeldOwner) bool {
	if owner.PID <= 0 || owner.PID >= 1<<32 || owner.DatabaseOID <= 0 || owner.DatabaseOID >= 1<<32 ||
		owner.RoleOID <= 0 || owner.RoleOID >= 1<<32 || owner.DatabaseName == "" || owner.RoleName == "" ||
		len(owner.Marker) != 54 || !strings.HasPrefix(owner.Marker, "rereply-plan-lock-") {
		return false
	}
	// A canonical UUIDv4 is nonzero by its version nibble. Validate its
	// lexical form without adding a package initializer to every seed closure.
	id := strings.TrimPrefix(owner.Marker, "rereply-plan-lock-")
	for i := 0; i < len(id); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if id[i] != '-' {
				return false
			}
		} else if (id[i] < '0' || id[i] > '9') && (id[i] < 'a' || id[i] > 'f') {
			return false
		}
	}
	return id[14] == '4' && (id[19] == '8' || id[19] == '9' || id[19] == 'a' || id[19] == 'b')
}

// ObservePlanSeedsOnHeldOwner admits the explicitly bound native migration
// owner, including a managed owner's CREATEROLE/BYPASSRLS/REPLICATION
// attributes. The ordinary ObservePlanSeeds API retains its strict role
// restrictions. This path additionally requires an exact live held-lock
// witness, and actual database ownership for a privileged owner; superuser,
// transaction, replication-origin and table-policy checks remain.
func ObservePlanSeedsOnHeldOwner(ctx context.Context, tx pgx.Tx, owner PlanSeedHeldOwner) (PlanSeedObservations, error) {
	return observePlanSeeds(ctx, tx, &owner)
}

func planSeedHeldOwnerSession(ctx context.Context, tx pgx.Tx, owner PlanSeedHeldOwner) error {
	if !validPlanSeedHeldOwner(owner) {
		return errPlanSeedSession
	}
	// Statistics snapshots must not turn the second check into a stale first
	// witness. This does not alter database data or the transaction snapshot.
	if _, err := tx.Exec(ctx, "SELECT pg_catalog.pg_stat_clear_snapshot()"); err != nil {
		return errPlanSeedSession
	}
	var eligible bool
	err := tx.QueryRow(ctx, `WITH marked AS (
 SELECT a.pid::bigint AS pid,a.datid::bigint AS database_oid,a.datname::text AS database_name,
 a.usesysid::bigint AS role_oid,a.usename::text AS role_name
 FROM pg_catalog.pg_stat_activity a WHERE a.application_name=$1
), held AS (
 SELECT l.pid::bigint AS pid,l.database::bigint AS database_oid FROM pg_catalog.pg_locks l
 WHERE l.locktype='advisory' AND l.mode='ExclusiveLock' AND l.granted AND l.objsubid=2
 AND l.database=(SELECT oid FROM pg_catalog.pg_database WHERE datname=pg_catalog.current_database())
 AND l.classid::bigint=(pg_catalog.hashtext(pg_catalog.current_database())::bigint & 4294967295)
 AND l.objid::bigint=1380270905
)
 SELECT current_user=session_user AND current_user::text=$2 AND r.oid::bigint=$3
 AND NOT r.rolsuper
 AND d.datname::text=$4 AND d.oid::bigint=$5
 AND ((NOT r.rolbypassrls AND NOT r.rolcreaterole AND NOT r.rolreplication) OR d.datdba=r.oid)
 AND pg_catalog.pg_backend_pid()::bigint<>$6
 AND pg_catalog.current_setting('application_name')='rereply-plan-owner'
 AND (SELECT count(*) FROM marked)=1 AND (SELECT count(*) FROM held)=1
 AND (SELECT count(*) FROM marked m JOIN held h ON h.pid=m.pid AND h.database_oid=m.database_oid
  WHERE m.pid=$6 AND m.database_oid=$5 AND m.database_name=$4 AND m.role_oid=$3 AND m.role_name=$2)=1
 FROM pg_catalog.pg_roles r JOIN pg_catalog.pg_database d ON d.datname=pg_catalog.current_database()
 WHERE r.rolname=current_user`, owner.Marker, owner.RoleName, owner.RoleOID, owner.DatabaseName, owner.DatabaseOID, owner.PID).Scan(&eligible)
	if err != nil || !eligible || ctx.Err() != nil {
		return errPlanSeedSession
	}
	return nil
}

// SeedDiagnosticReport is deliberately separate from PlanSeedObservations. It
// grants no migration admission and describes only two fixed seed predicates.
// Its only database-derived strings are selected from compiled permission keys.
type SeedDiagnosticReport struct {
	Version            int                       `json:"version"`
	Purpose            string                    `json:"purpose"`
	DiagnosticOnly     bool                      `json:"diagnostic_only"`
	Writes             bool                      `json:"writes"`
	CompleteVisibility bool                      `json:"complete_visibility"`
	Permissions        SeedDiagnosticPermissions `json:"permissions"`
	Reseller           SeedDiagnosticReseller    `json:"reseller"`
}

type SeedDiagnosticPermission struct {
	Resource        string `json:"resource"`
	Action          string `json:"action"`
	DeletedConflict bool   `json:"deleted_conflict"`
}

type SeedDiagnosticPermissions struct {
	State         string                     `json:"state"`
	ExpectedCount int                        `json:"expected_count"`
	Missing       []SeedDiagnosticPermission `json:"missing"`
}

type SeedDiagnosticReseller struct {
	State                         string `json:"state"`
	PlatformRows                  int64  `json:"platform_row_count"`
	PlatformInactiveOrDeleted     int64  `json:"platform_inactive_or_deleted_count"`
	OrganizationsWithoutReseller  int64  `json:"live_organizations_without_reseller_count"`
	MissingOwnerMembership        int64  `json:"missing_active_superadmin_owner_membership_count"`
	InvalidOwnerMembership        int64  `json:"inactive_deleted_or_nonowner_membership_count"`
	DuplicateOwnerMembershipGroup int64  `json:"duplicate_owner_membership_group_count"`
}

const SeedDiagnosticApplication = "rereply-seed-diagnostic"
const seedDiagnosticPermissionLimit = 141

// ObserveSeedDiagnostic requires a native database owner and complete visibility
// even when that managed owner has BYPASSRLS, CREATEROLE or REPLICATION. This is a
// separate read-only diagnostic admission, never the planner's held-owner path.
// The caller owns the fixed deadline, physical-session witness and disposal.
func ObserveSeedDiagnostic(ctx context.Context, tx pgx.Tx) (SeedDiagnosticReport, error) {
	var result SeedDiagnosticReport
	if err := seedDiagnosticSession(ctx, tx); err != nil {
		return result, err
	}
	if err := planSeedTables(ctx, tx, "permissions", "resellers", "reseller_members", "organizations", "users"); err != nil {
		return result, err
	}
	permissions, err := diagnoseSeedPermissions(ctx, tx)
	if err != nil {
		return result, err
	}
	reseller, err := diagnoseSeedReseller(ctx, tx)
	if err != nil {
		return result, err
	}
	if err := seedDiagnosticSession(ctx, tx); err != nil {
		return result, err
	}
	return SeedDiagnosticReport{Version: 1, Purpose: "seed_diagnostic", DiagnosticOnly: true,
		CompleteVisibility: true, Permissions: permissions, Reseller: reseller}, nil
}

func seedDiagnosticSession(ctx context.Context, tx pgx.Tx) error {
	if ctx == nil || tx == nil || ctx.Err() != nil {
		return errPlanSeedSession
	}
	if _, ok := ctx.Deadline(); !ok {
		return errPlanSeedSession
	}
	var eligible bool
	err := tx.QueryRow(ctx, `SELECT current_user=session_user AND NOT r.rolsuper
 AND d.datdba=r.oid
 AND pg_catalog.current_setting('application_name')=$1
 AND pg_catalog.current_setting('server_version_num')::integer/10000=17
 AND NOT pg_catalog.pg_is_in_recovery()
 AND pg_catalog.current_setting('transaction_read_only')='on'
 AND pg_catalog.current_setting('transaction_isolation')='repeatable read'
 AND pg_catalog.current_setting('search_path')='pg_catalog'
 AND pg_catalog.current_setting('server_encoding')='UTF8'
 AND pg_catalog.current_setting('session_replication_role')='origin'
 AND pg_catalog.current_setting('statement_timeout')::interval > interval '0 seconds'
 AND pg_catalog.current_setting('statement_timeout')::interval <= interval '3 seconds'
 AND pg_catalog.current_setting('lock_timeout')::interval > interval '0 seconds'
 AND pg_catalog.current_setting('lock_timeout')::interval <= interval '1 second'
 AND pg_catalog.current_setting('idle_in_transaction_session_timeout')::interval > interval '0 seconds'
 AND pg_catalog.current_setting('idle_in_transaction_session_timeout')::interval <= interval '5 seconds'
 AND (SELECT a.usename=session_user AND a.usesysid=r.oid AND a.datid=d.oid
 FROM pg_catalog.pg_stat_activity a WHERE a.pid=pg_catalog.pg_backend_pid())
 FROM pg_catalog.pg_roles r JOIN pg_catalog.pg_database d ON d.datname=pg_catalog.current_database()
 WHERE r.rolname=current_user`, SeedDiagnosticApplication).Scan(&eligible)
	if err != nil || !eligible || ctx.Err() != nil {
		return errPlanSeedSession
	}
	return nil
}

func seedDiagnosticExpectedPermissions(source []models.Permission) ([]SeedDiagnosticPermission, error) {
	if len(source) == 0 || len(source) > seedDiagnosticPermissionLimit {
		return nil, errPlanSeedUnavailable
	}
	pairs := make([]SeedDiagnosticPermission, 0, len(source))
	seen := make(map[string]bool, len(source))
	for _, p := range source {
		for _, field := range []string{p.Resource, p.Action} {
			if field == "" || len(field) > 64 || strings.Trim(field, "abcdefghijklmnopqrstuvwxyz0123456789._-") != "" {
				return nil, errPlanSeedUnavailable
			}
		}
		key := p.Resource + ":" + p.Action
		if seen[key] {
			return nil, errPlanSeedUnavailable
		}
		seen[key] = true
		pairs = append(pairs, SeedDiagnosticPermission{Resource: p.Resource, Action: p.Action})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Resource != pairs[j].Resource {
			return pairs[i].Resource < pairs[j].Resource
		}
		return pairs[i].Action < pairs[j].Action
	})
	return pairs, nil
}

func seedDiagnosticPredicateState(drift bool, err error) (string, error) {
	if errors.Is(err, errPlanSeedConflict) {
		return "conflict", nil
	}
	if err != nil {
		return "", errPlanSeedUnavailable
	}
	if drift {
		return "drift", nil
	}
	return "clear", nil
}

func diagnoseSeedPermissions(ctx context.Context, tx pgx.Tx) (SeedDiagnosticPermissions, error) {
	var result SeedDiagnosticPermissions
	pairs, err := seedDiagnosticExpectedPermissions(models.DefaultPermissions())
	if err != nil {
		return result, err
	}
	raw, err := json.Marshal(pairs)
	if err != nil {
		return result, errPlanSeedUnavailable
	}
	result.ExpectedCount = len(pairs)
	result.Missing = make([]SeedDiagnosticPermission, 0)
	rows, err := tx.Query(ctx, `WITH expected AS (
 SELECT * FROM pg_catalog.jsonb_to_recordset($1::jsonb) AS e(resource text,action text)
), missing AS (SELECT e.* FROM expected e WHERE NOT EXISTS (
 SELECT 1 FROM public.permissions p WHERE p.deleted_at IS NULL AND p.resource=e.resource AND p.action=e.action))
 SELECT e.resource,e.action,EXISTS(SELECT 1 FROM public.permissions p
 WHERE p.resource=e.resource AND p.action=e.action AND p.deleted_at IS NOT NULL)
 FROM missing e ORDER BY e.resource COLLATE "C",e.action COLLATE "C" LIMIT 142`, string(raw))
	if err != nil {
		return result, errPlanSeedUnavailable
	}
	defer rows.Close()
	allowed := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		allowed[p.Resource+":"+p.Action] = true
	}
	conflict := false
	for rows.Next() {
		var p SeedDiagnosticPermission
		if rows.Scan(&p.Resource, &p.Action, &p.DeletedConflict) != nil || len(result.Missing) >= seedDiagnosticPermissionLimit {
			return SeedDiagnosticPermissions{}, errPlanSeedUnavailable
		}
		key := p.Resource + ":" + p.Action
		if !allowed[key] {
			return SeedDiagnosticPermissions{}, errPlanSeedUnavailable
		}
		delete(allowed, key)
		conflict = conflict || p.DeletedConflict
		result.Missing = append(result.Missing, p)
	}
	if rows.Err() != nil || ctx.Err() != nil {
		return SeedDiagnosticPermissions{}, errPlanSeedUnavailable
	}
	rows.Close()
	result.State = "clear"
	if len(result.Missing) != 0 {
		result.State = "drift"
	}
	if conflict {
		result.State = "conflict"
	}
	// Keep the unchanged authoritative predicate as an independent parity check.
	drift, predicateErr := planPermissions(ctx, tx)
	canonical, err := seedDiagnosticPredicateState(drift, predicateErr)
	if err != nil || canonical != result.State {
		return SeedDiagnosticPermissions{}, errPlanSeedUnavailable
	}
	return result, nil
}

func diagnoseSeedReseller(ctx context.Context, tx pgx.Tx) (SeedDiagnosticReseller, error) {
	var result SeedDiagnosticReseller
	err := tx.QueryRow(ctx, `WITH platform AS (
 SELECT id,deleted_at,status FROM public.resellers WHERE slug=$1
), owner_pairs AS (
 SELECT r.id AS reseller_id,u.id AS user_id FROM platform r CROSS JOIN public.users u
 WHERE u.deleted_at IS NULL AND u.is_super_admin AND u.is_active
), duplicate_groups AS (
 SELECT m.reseller_id,m.user_id FROM public.reseller_members m
 JOIN owner_pairs p ON p.reseller_id=m.reseller_id AND p.user_id=m.user_id
 GROUP BY m.reseller_id,m.user_id HAVING pg_catalog.count(*)>1
)
 SELECT (SELECT pg_catalog.count(*) FROM platform),
 (SELECT pg_catalog.count(*) FROM platform WHERE deleted_at IS NOT NULL OR status IS DISTINCT FROM $2),
 (SELECT pg_catalog.count(*) FROM public.organizations WHERE deleted_at IS NULL AND reseller_id IS NULL),
 (SELECT pg_catalog.count(*) FROM owner_pairs p WHERE NOT EXISTS (
 SELECT 1 FROM public.reseller_members m WHERE m.reseller_id=p.reseller_id AND m.user_id=p.user_id)),
 (SELECT pg_catalog.count(*) FROM public.reseller_members m JOIN owner_pairs p
 ON p.reseller_id=m.reseller_id AND p.user_id=m.user_id
 WHERE m.deleted_at IS NOT NULL OR m.role IS DISTINCT FROM $3 OR m.is_active IS DISTINCT FROM true),
 (SELECT pg_catalog.count(*) FROM duplicate_groups)`, PlatformResellerSlug, models.ResellerStatusActive, models.ResellerRoleOwner).
		Scan(&result.PlatformRows, &result.PlatformInactiveOrDeleted, &result.OrganizationsWithoutReseller,
			&result.MissingOwnerMembership, &result.InvalidOwnerMembership, &result.DuplicateOwnerMembershipGroup)
	if err != nil || ctx.Err() != nil {
		return SeedDiagnosticReseller{}, errPlanSeedUnavailable
	}
	for _, count := range []int64{result.PlatformRows, result.PlatformInactiveOrDeleted, result.OrganizationsWithoutReseller,
		result.MissingOwnerMembership, result.InvalidOwnerMembership, result.DuplicateOwnerMembershipGroup} {
		if count < 0 {
			return SeedDiagnosticReseller{}, errPlanSeedUnavailable
		}
	}
	result.State = "clear"
	if result.PlatformRows == 0 || result.PlatformInactiveOrDeleted != 0 || result.OrganizationsWithoutReseller != 0 ||
		result.MissingOwnerMembership != 0 || result.InvalidOwnerMembership != 0 {
		result.State = "drift"
	}
	if result.PlatformRows > 1 || result.DuplicateOwnerMembershipGroup != 0 {
		result.State = "conflict"
	}
	drift, predicateErr := planPlatformReseller(ctx, tx)
	canonical, err := seedDiagnosticPredicateState(drift, predicateErr)
	if err != nil || canonical != result.State {
		return SeedDiagnosticReseller{}, errPlanSeedUnavailable
	}
	return result, nil
}

const IdentityInventoryApplication = "rereply-identity-inventory"
const IdentityInventoryReportByteLimit = 65536
const identityInventoryHoldLimit = 10000
const identityInventoryMemberLimit = 100000
const identityInventoryReceiptLimit = 20000
const identityInventoryCycleLimit = 256
const identityInventoryAggregateLimit = 512

// This is exactly unicode.IsSpace's White_Space set used by strings.TrimSpace.
// It deliberately excludes BOM/zero-width space. UTF8 and byte length are
// separately required; noncanonical stored keys are not normalized for grouping.
const identityInventoryTrimSpace = "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"

type IdentityInventoryScope struct{ OrganizationID, AccountID string }

// The witness is deliberately unexported and has no JSON-visible fields. It is
// a scope comparison, not a sender identifier, authorization or signed receipt.
type IdentityInventoryWitness struct{ digest string }

type IdentityInventoryQueryOne struct {
	Ordinal          int    `json:"ordinal"`
	CycleRelation    string `json:"cycle_relation"`
	Holds            int64  `json:"holds"`
	StoredMemberRows int64  `json:"stored_member_rows"`
}

type IdentityInventoryCounts struct {
	Holds                       int64 `json:"holds"`
	Supported                   int64 `json:"supported"`
	Unsupported                 int64 `json:"unsupported"`
	SupportUnknown              int64 `json:"support_unknown"`
	Open                        int64 `json:"open"`
	FutureRouting               int64 `json:"future_routing"`
	SupersededLatest            int64 `json:"superseded_by_latest"`
	SupersededCycle             int64 `json:"superseded_by_cycle"`
	OtherDisposition            int64 `json:"other_disposition"`
	CurrentCycle                int64 `json:"current_cycle"`
	OlderCycle                  int64 `json:"older_cycle"`
	FutureOrInvalidCycle        int64 `json:"future_or_invalid_cycle"`
	ProtocolV1                  int64 `json:"protocol_v1"`
	OtherProtocol               int64 `json:"other_protocol"`
	RecordedVerifiedClaimShape  int64 `json:"recorded_verified_claim_shape"`
	ClaimShapeUnknown           int64 `json:"claim_shape_unknown"`
	DirectPresent               int64 `json:"direct_present"`
	PhoneCandidate              int64 `json:"phone_candidate"`
	ParentOnly                  int64 `json:"parent_only"`
	NoAddressCandidate          int64 `json:"no_address_candidate"`
	UnsupportedWithDirect       int64 `json:"unsupported_with_direct_legacy"`
	NoMemberSnapshot            int64 `json:"no_member_snapshot"`
	OldCycleOpen                int64 `json:"old_cycle_open"`
	DirectSenderUnits           int64 `json:"direct_sender_units"`
	PhoneSenderUnits            int64 `json:"phone_sender_units"`
	UnknownSenderHoldIncidences int64 `json:"unknown_sender_hold_incidences"`
	MemberRows                  int64 `json:"member_rows"`
	MemberSelectorInvalid       int64 `json:"member_selector_invalid"`
	MemberDuplicateRows         int64 `json:"member_duplicate_rows"`
	MemberCountMismatchHolds    int64 `json:"member_count_mismatch_holds"`
	ReceiptRows                 int64 `json:"receipt_rows"`
	ReceiptLinkedToHold         int64 `json:"receipt_linked_to_hold"`
	ReceiptAccountOnly          int64 `json:"receipt_account_only"`
	ReceiptRecordedCopyShape    int64 `json:"receipt_recorded_copy_shape"`
	ReceiptShapeUnknown         int64 `json:"receipt_shape_unknown"`
	ReceiptDeleted              int64 `json:"receipt_deleted"`
	ReceiptWrongAccount         int64 `json:"receipt_wrong_account"`
	ReceiptInvalidProtocol      int64 `json:"receipt_invalid_protocol"`
	ReceiptInvalidSignature     int64 `json:"receipt_invalid_signature"`
	ReceiptInvalidStatus        int64 `json:"receipt_invalid_status"`
	ReceiptDuplicateRows        int64 `json:"receipt_duplicate_rows"`
}

// These histograms overlap. A shared phone is not proof of a foreign owner.
type IdentityInventoryOverlap struct {
	DirectUnitsWithMultiplePhones      int64 `json:"direct_units_with_multiple_phones"`
	DirectUnitsWithMultipleParents     int64 `json:"direct_units_with_multiple_parents"`
	PhoneGroupsWithMultipleDirectUnits int64 `json:"phone_groups_with_multiple_direct_units"`
	PhoneOnlyUnitsAlsoUsedByDirect     int64 `json:"phone_only_units_also_used_by_direct"`
}

type IdentityInventoryReasons struct {
	UnsupportedNoDirect                    int64 `json:"unsupported_no_direct"`
	UnsupportedWithDirectLegacy            int64 `json:"unsupported_with_direct_legacy"`
	SupportedCurrentClaim                  int64 `json:"supported_current_claim"`
	OldCycleOpen                           int64 `json:"old_cycle_open"`
	NoMemberSnapshot                       int64 `json:"no_member_snapshot"`
	MemberCountOrSelectorShapeInconsistent int64 `json:"member_count_or_selector_shape_inconsistent"`
	ClaimProvenanceUnknown                 int64 `json:"claim_provenance_unknown"`
}

type IdentityInventoryReport struct {
	Version                int                         `json:"version"`
	Purpose                string                      `json:"purpose"`
	Scope                  string                      `json:"scope"`
	ReasonSource           string                      `json:"reason_source"`
	ReasonsOverlap         bool                        `json:"derived_reasons_overlap"`
	ScopeFailurePolicy     string                      `json:"scope_failure_policy"`
	CurrentDecidability    string                      `json:"current_decidability"`
	RecordedAuthentication string                      `json:"recorded_authentication"`
	CompleteVisibility     bool                        `json:"complete_visibility"`
	ShadowState            string                      `json:"shadow_state"`
	Counts                 IdentityInventoryCounts     `json:"counts"`
	OverlappingHistograms  IdentityInventoryOverlap    `json:"overlapping_histograms"`
	DerivedReasons         IdentityInventoryReasons    `json:"derived_reasons"`
	QueryOne               []IdentityInventoryQueryOne `json:"query_one"`
}

func identityInventoryScopeValid(scope IdentityInventoryScope) bool {
	for _, value := range []string{scope.OrganizationID, scope.AccountID} {
		if len(value) != 36 || value == "00000000-0000-0000-0000-000000000000" {
			return false
		}
		for i, c := range value {
			if i == 8 || i == 13 || i == 18 || i == 23 {
				if c != '-' {
					return false
				}
			} else if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}

func identityInventoryAdmission(ctx context.Context, tx pgx.Tx, scope IdentityInventoryScope) error {
	if !identityInventoryScopeValid(scope) {
		return errPlanSeedSession
	}
	if err := identityInventorySession(ctx, tx); err != nil {
		return err
	}
	if err := planSeedTables(ctx, tx, "organizations"); err != nil {
		return errPlanSeedUnavailable
	}
	for _, table := range []string{"whatsapp_accounts", "whatsapp_coexistence_states", "whatsapp_identity_review_holds", "whatsapp_identity_review_members", "inbound_events", "channel_accounts"} {
		if err := planSeedTenantTable(ctx, tx, table); err != nil {
			return errPlanSeedUnavailable
		}
	}
	return nil
}

// All selector/private binding values stay in PostgreSQL. The bounded tuple is
// hashed there and can only be compared within this command's physical session.
// Zero claims is observed absence. Every present row must bind this account by
// BOTH exact selectors; even one contradictory/missing binding refuses. Only a
// wrong channel/provider on an exact binding, or deletion, is a fixed anomaly.
// Two claims (including a deleted predecessor) always refuse ambiguity.
func identityInventoryScopeWitness(ctx context.Context, tx pgx.Tx, scope IdentityInventoryScope) (IdentityInventoryWitness, error) {
	var digest *string
	err := tx.QueryRow(ctx, `WITH
 o AS MATERIALIZED (SELECT id,deleted_at FROM public.organizations WHERE id=$1::uuid LIMIT 2),
 a AS MATERIALIZED (SELECT id,organization_id,phone_id,status,is_smb,updated_at,deleted_at FROM public.whatsapp_accounts WHERE id=$2::uuid LIMIT 2),
 s AS MATERIALIZED (SELECT id,organization_id,whats_app_account_id,onboarding_cycle,version,lifecycle_status,updated_at FROM public.whatsapp_coexistence_states WHERE organization_id=$1::uuid AND whats_app_account_id=$2::uuid LIMIT 2),
 c AS MATERIALIZED (SELECT id,organization_id,channel,provider,external_account_id,metadata->>'legacy_account_id' AS legacy_account_id,deleted_at FROM public.channel_accounts WHERE organization_id=$1::uuid AND (external_account_id='legacy-account:'||$2::text OR metadata->>'legacy_account_id'=$2::text) LIMIT 2)
 SELECT CASE WHEN (SELECT count(*) FROM o)=1 AND (SELECT count(*) FROM a)=1
 AND (SELECT count(*) FROM s)=1 AND (SELECT count(*) FROM c)<=1
 AND EXISTS(SELECT 1 FROM o WHERE deleted_at IS NULL)
 AND EXISTS(SELECT 1 FROM a WHERE organization_id=$1::uuid AND deleted_at IS NULL
 AND phone_id IS NOT NULL AND octet_length(phone_id)<=255 AND status IS NOT NULL AND octet_length(status)<=64
 AND is_smb IS NOT NULL AND updated_at IS NOT NULL)
 AND EXISTS(SELECT 1 FROM s WHERE id IS NOT NULL AND id<>'00000000-0000-0000-0000-000000000000'::uuid AND onboarding_cycle>0 AND version>0
 AND lifecycle_status IN ('unknown','connected','disconnected','offboarded') AND updated_at IS NOT NULL)
 AND NOT EXISTS(SELECT 1 FROM c WHERE id IS NULL OR id='00000000-0000-0000-0000-000000000000'::uuid OR channel IS NULL OR provider IS NULL OR external_account_id IS NULL
 OR octet_length(channel)>64 OR octet_length(provider)>64 OR octet_length(external_account_id)>255 OR octet_length(COALESCE(legacy_account_id,''))>255)
 AND NOT EXISTS(SELECT 1 FROM c WHERE external_account_id IS DISTINCT FROM 'legacy-account:'||$2::text OR legacy_account_id IS DISTINCT FROM $2::text)
 THEN pg_catalog.encode(pg_catalog.sha256(pg_catalog.convert_to(pg_catalog.jsonb_build_array(
 (SELECT pg_catalog.to_jsonb(o) FROM o),(SELECT pg_catalog.to_jsonb(a) FROM a),
 (SELECT pg_catalog.to_jsonb(s) FROM s),(SELECT pg_catalog.to_jsonb(c) FROM c))::text,'UTF8')),'hex') END`, scope.OrganizationID, scope.AccountID).Scan(&digest)
	if err != nil || digest == nil || len(*digest) != 64 || strings.Trim(*digest, "0123456789abcdef") != "" || ctx.Err() != nil {
		return IdentityInventoryWitness{}, errPlanSeedUnavailable
	}
	return IdentityInventoryWitness{digest: *digest}, nil
}

func RecheckIdentityInventoryScope(ctx context.Context, tx pgx.Tx, scope IdentityInventoryScope, before IdentityInventoryWitness) error {
	if err := identityInventoryAdmission(ctx, tx, scope); err != nil {
		return err
	}
	after, err := identityInventoryScopeWitness(ctx, tx, scope)
	if err != nil || before.digest == "" || before != after {
		return errPlanSeedUnavailable
	}
	return identityInventorySession(ctx, tx)
}

// This shared literal defines the exact bounded cohorts for preflight and the
// final aggregation. It reads no body, headers, error text or provider payload.
const identityInventoryCohortSQL = `WITH
 h AS MATERIALIZED (SELECT id,organization_id,whats_app_account_id,onboarding_cycle,protocol_version,supported,direct_primary_bsuid,parent_bsuid,phone,principal_generation,semantic_claim_digest,selector_body_digest,verified_event_digest,verified_event_provenance,member_count,member_digest,version,disposition
 FROM public.whatsapp_identity_review_holds WHERE organization_id=$1::uuid AND whats_app_account_id=$2::uuid LIMIT 10001),
 m AS MATERIALIZED (SELECT hold_id,contact_id,selector_reasons FROM public.whatsapp_identity_review_members WHERE organization_id=$1::uuid AND hold_id IN (SELECT id FROM h) LIMIT 100001),
 c AS MATERIALIZED (SELECT id,channel,provider,external_account_id,metadata->>'legacy_account_id' AS legacy_account_id,deleted_at FROM public.channel_accounts WHERE organization_id=$1::uuid AND (external_account_id='legacy-account:'||$2::text OR metadata->>'legacy_account_id'=$2::text) LIMIT 2),
 r AS MATERIALIZED (SELECT id,channel_account_id,review_hold_id,protocol,event_type,status,signature_valid,deleted_at FROM public.inbound_events WHERE organization_id=$1::uuid AND (review_hold_id IN (SELECT id FROM h) OR channel_account_id IN (SELECT id FROM c)) LIMIT 20001),
 cycles AS MATERIALIZED (SELECT onboarding_cycle FROM h GROUP BY onboarding_cycle LIMIT 257)
`

func ObserveIdentityInventory(ctx context.Context, tx pgx.Tx, scope IdentityInventoryScope) (IdentityInventoryReport, IdentityInventoryWitness, error) {
	var empty IdentityInventoryReport
	if err := identityInventoryAdmission(ctx, tx, scope); err != nil {
		return empty, IdentityInventoryWitness{}, err
	}
	witness, err := identityInventoryScopeWitness(ctx, tx, scope)
	if err != nil {
		return empty, IdentityInventoryWitness{}, err
	}
	var holds, uniqueHolds, members, receipts, cycles int64
	err = tx.QueryRow(ctx, identityInventoryCohortSQL+`SELECT (SELECT count(*) FROM h),(SELECT count(DISTINCT id) FROM h),(SELECT count(*) FROM m),(SELECT count(*) FROM r),(SELECT count(*) FROM cycles)`, scope.OrganizationID, scope.AccountID).Scan(&holds, &uniqueHolds, &members, &receipts, &cycles)
	if err != nil || holds != uniqueHolds || holds > identityInventoryHoldLimit || members > identityInventoryMemberLimit || receipts > identityInventoryReceiptLimit || cycles > identityInventoryCycleLimit || ctx.Err() != nil {
		return empty, IdentityInventoryWitness{}, errPlanSeedUnavailable
	}
	var raw *string
	err = tx.QueryRow(ctx, identityInventoryCohortSQL+identityInventoryAggregateSQL, scope.OrganizationID, scope.AccountID, identityInventoryTrimSpace).Scan(&raw)
	if err != nil || raw == nil || len(*raw) > IdentityInventoryReportByteLimit || ctx.Err() != nil {
		return empty, IdentityInventoryWitness{}, errPlanSeedUnavailable
	}
	report, err := DecodeIdentityInventoryReport([]byte(*raw))
	if err != nil || report.Counts.Holds != holds || report.Counts.MemberRows != members || report.Counts.ReceiptRows != receipts {
		return empty, IdentityInventoryWitness{}, errPlanSeedUnavailable
	}
	if err := identityInventorySession(ctx, tx); err != nil {
		return empty, IdentityInventoryWitness{}, err
	}
	return report, witness, nil
}

// The public decoder is closed: no arbitrary map keys, raw identifiers or
// unknown categories can pass through a successful report. Input and output
// sizes are bounded separately from SQL row caps and deadlines.
func DecodeIdentityInventoryReport(raw []byte) (IdentityInventoryReport, error) {
	var r IdentityInventoryReport
	if len(raw) == 0 || len(raw) > IdentityInventoryReportByteLimit {
		return r, errPlanSeedUnavailable
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r); err != nil || strings.TrimSpace(string(raw[decoder.InputOffset():])) != "" {
		return IdentityInventoryReport{}, errPlanSeedUnavailable
	}
	expected, err := json.Marshal(r)
	if err != nil || !identityInventoryJSONFields(raw, expected) {
		return IdentityInventoryReport{}, errPlanSeedUnavailable
	}
	if r.Version != 1 || r.Purpose != "identity_inventory" || r.Scope != "selected_account" || r.ReasonSource != "derived_shape" || !r.ReasonsOverlap || r.ScopeFailurePolicy != "whole_report_refusal" || r.CurrentDecidability != "not_observed" || r.RecordedAuthentication != "stored_shape_not_fresh_verification" || !r.CompleteVisibility {
		return IdentityInventoryReport{}, errPlanSeedUnavailable
	}
	c := r.Counts
	for _, n := range []int64{r.DerivedReasons.UnsupportedNoDirect, r.DerivedReasons.UnsupportedWithDirectLegacy, r.DerivedReasons.SupportedCurrentClaim, r.DerivedReasons.OldCycleOpen, r.DerivedReasons.NoMemberSnapshot, r.DerivedReasons.MemberCountOrSelectorShapeInconsistent, r.DerivedReasons.ClaimProvenanceUnknown} {
		if n < 0 || n > c.Holds {
			return IdentityInventoryReport{}, errPlanSeedUnavailable
		}
	}
	if r.DerivedReasons.UnsupportedWithDirectLegacy != c.UnsupportedWithDirect || r.DerivedReasons.OldCycleOpen != c.OldCycleOpen || r.DerivedReasons.NoMemberSnapshot != c.NoMemberSnapshot || r.DerivedReasons.ClaimProvenanceUnknown != c.ClaimShapeUnknown {
		return IdentityInventoryReport{}, errPlanSeedUnavailable
	}
	switch r.ShadowState {
	case "absent", "live_exact", "deleted_exact", "contradictory":
	default:
		return IdentityInventoryReport{}, errPlanSeedUnavailable
	}
	for _, n := range []int64{c.Holds, c.Supported, c.Unsupported, c.SupportUnknown, c.Open, c.FutureRouting, c.SupersededLatest, c.SupersededCycle, c.OtherDisposition, c.CurrentCycle, c.OlderCycle, c.FutureOrInvalidCycle, c.ProtocolV1, c.OtherProtocol, c.RecordedVerifiedClaimShape, c.ClaimShapeUnknown, c.DirectPresent, c.PhoneCandidate, c.ParentOnly, c.NoAddressCandidate, c.UnsupportedWithDirect, c.NoMemberSnapshot, c.OldCycleOpen, c.DirectSenderUnits, c.PhoneSenderUnits, c.UnknownSenderHoldIncidences, c.MemberRows, c.MemberSelectorInvalid, c.MemberDuplicateRows, c.MemberCountMismatchHolds, c.ReceiptRows, c.ReceiptLinkedToHold, c.ReceiptAccountOnly, c.ReceiptRecordedCopyShape, c.ReceiptShapeUnknown, c.ReceiptDeleted, c.ReceiptWrongAccount, c.ReceiptInvalidProtocol, c.ReceiptInvalidSignature, c.ReceiptInvalidStatus, c.ReceiptDuplicateRows, r.OverlappingHistograms.DirectUnitsWithMultiplePhones, r.OverlappingHistograms.DirectUnitsWithMultipleParents, r.OverlappingHistograms.PhoneGroupsWithMultipleDirectUnits, r.OverlappingHistograms.PhoneOnlyUnitsAlsoUsedByDirect} {
		if n < 0 || n > identityInventoryMemberLimit {
			return IdentityInventoryReport{}, errPlanSeedUnavailable
		}
	}
	if c.Holds > identityInventoryHoldLimit || c.ReceiptRows > identityInventoryReceiptLimit ||
		c.Supported+c.Unsupported+c.SupportUnknown != c.Holds || c.Open+c.FutureRouting+c.SupersededLatest+c.SupersededCycle+c.OtherDisposition != c.Holds ||
		c.CurrentCycle+c.OlderCycle+c.FutureOrInvalidCycle != c.Holds || c.ProtocolV1+c.OtherProtocol != c.Holds ||
		c.RecordedVerifiedClaimShape+c.ClaimShapeUnknown != c.Holds || c.DirectPresent+c.PhoneCandidate+c.ParentOnly+c.NoAddressCandidate+c.ClaimShapeUnknown != c.Holds ||
		c.ReceiptLinkedToHold+c.ReceiptAccountOnly != c.ReceiptRows || c.ReceiptRecordedCopyShape+c.ReceiptShapeUnknown != c.ReceiptRows ||
		c.DirectSenderUnits+c.PhoneSenderUnits+c.UnknownSenderHoldIncidences > c.Holds ||
		c.DirectSenderUnits > c.DirectPresent || c.PhoneSenderUnits > c.PhoneCandidate || c.UnknownSenderHoldIncidences != c.ClaimShapeUnknown+c.ParentOnly+c.NoAddressCandidate ||
		c.ReceiptDeleted > c.ReceiptRows || c.ReceiptWrongAccount > c.ReceiptRows || c.ReceiptInvalidProtocol > c.ReceiptRows || c.ReceiptInvalidSignature > c.ReceiptRows || c.ReceiptInvalidStatus > c.ReceiptRows ||
		r.OverlappingHistograms.DirectUnitsWithMultiplePhones > c.DirectSenderUnits || r.OverlappingHistograms.DirectUnitsWithMultipleParents > c.DirectSenderUnits || r.OverlappingHistograms.PhoneGroupsWithMultipleDirectUnits > c.DirectPresent || r.OverlappingHistograms.PhoneOnlyUnitsAlsoUsedByDirect > c.PhoneSenderUnits ||
		c.MemberCountMismatchHolds > c.Holds || c.MemberSelectorInvalid > c.MemberRows || c.MemberDuplicateRows > c.MemberRows || c.ReceiptDuplicateRows > c.ReceiptRows ||
		r.DerivedReasons.MemberCountOrSelectorShapeInconsistent < c.MemberCountMismatchHolds ||
		r.QueryOne == nil || len(r.QueryOne) > identityInventoryCycleLimit || len(r.QueryOne)+4 > identityInventoryAggregateLimit {
		return IdentityInventoryReport{}, errPlanSeedUnavailable
	}
	var queryHolds int64
	for i, row := range r.QueryOne {
		if row.Ordinal != i+1 || row.Holds <= 0 || row.Holds > c.Holds || row.StoredMemberRows < row.Holds || row.StoredMemberRows > 4294967295*int64(identityInventoryHoldLimit) {
			return IdentityInventoryReport{}, errPlanSeedUnavailable
		}
		switch row.CycleRelation {
		case "current", "older", "future_invalid":
		default:
			return IdentityInventoryReport{}, errPlanSeedUnavailable
		}
		queryHolds += row.Holds
	}
	if queryHolds > c.Unsupported || queryHolds > c.Open {
		return IdentityInventoryReport{}, errPlanSeedUnavailable
	}
	return r, nil
}

// Compare only the closed typed object's field/array shape. This rejects a
// missing or null numeric field that encoding/json would otherwise turn into
// zero. The expected structure has a fixed depth and at most256 cycle rows.
func identityInventoryJSONFields(raw, expected []byte) bool {
	rawText, expectedText := strings.TrimSpace(string(raw)), strings.TrimSpace(string(expected))
	if rawText == "" || rawText == "null" || expectedText == "" {
		return false
	}
	switch expectedText[0] {
	case '{':
		var actual, schema map[string]json.RawMessage
		if json.Unmarshal(raw, &actual) != nil || json.Unmarshal(expected, &schema) != nil || len(actual) != len(schema) {
			return false
		}
		for key, value := range schema {
			if !identityInventoryJSONFields(actual[key], value) {
				return false
			}
		}
	case '[':
		var actual, schema []json.RawMessage
		if json.Unmarshal(raw, &actual) != nil || json.Unmarshal(expected, &schema) != nil || len(actual) != len(schema) {
			return false
		}
		for i, value := range schema {
			if !identityInventoryJSONFields(actual[i], value) {
				return false
			}
		}
	}
	return true
}

func identityInventorySession(ctx context.Context, tx pgx.Tx) error {
	if ctx == nil || tx == nil || ctx.Err() != nil {
		return errPlanSeedSession
	}
	if _, ok := ctx.Deadline(); !ok {
		return errPlanSeedSession
	}
	var eligible bool
	err := tx.QueryRow(ctx, `SELECT current_user=session_user AND NOT r.rolsuper
 AND d.datdba=r.oid
 AND pg_catalog.current_setting('application_name')=$1
 AND pg_catalog.current_setting('server_version_num')::integer/10000=17
 AND NOT pg_catalog.pg_is_in_recovery()
 AND pg_catalog.current_setting('transaction_read_only')='on'
 AND pg_catalog.current_setting('transaction_isolation')='repeatable read'
 AND pg_catalog.current_setting('search_path')='pg_catalog'
 AND pg_catalog.current_setting('server_encoding')='UTF8'
 AND pg_catalog.current_setting('session_replication_role')='origin'
 AND pg_catalog.current_setting('statement_timeout')::interval > interval '0 seconds'
 AND pg_catalog.current_setting('statement_timeout')::interval <= interval '3 seconds'
 AND pg_catalog.current_setting('lock_timeout')::interval > interval '0 seconds'
 AND pg_catalog.current_setting('lock_timeout')::interval <= interval '1 second'
 AND pg_catalog.current_setting('idle_in_transaction_session_timeout')::interval > interval '0 seconds'
 AND pg_catalog.current_setting('idle_in_transaction_session_timeout')::interval <= interval '5 seconds'
 AND (SELECT a.usename=session_user AND a.usesysid=r.oid AND a.datid=d.oid
 FROM pg_catalog.pg_stat_activity a WHERE a.pid=pg_catalog.pg_backend_pid())
 FROM pg_catalog.pg_roles r JOIN pg_catalog.pg_database d ON d.datname=pg_catalog.current_database()
 WHERE r.rolname=current_user`, IdentityInventoryApplication).Scan(&eligible)
	if err != nil || !eligible || ctx.Err() != nil {
		return errPlanSeedSession
	}
	return nil
}

const identityInventoryAggregateSQL = `,
 s AS MATERIALIZED (SELECT onboarding_cycle FROM public.whatsapp_coexistence_states WHERE organization_id=$1::uuid AND whats_app_account_id=$2::uuid),
 shaped AS MATERIALIZED (SELECT h.*,
 COALESCE(id<>'00000000-0000-0000-0000-000000000000'::uuid AND protocol_version=1 AND supported IS NOT NULL AND onboarding_cycle>0 AND onboarding_cycle<=(SELECT onboarding_cycle FROM s)
 AND direct_primary_bsuid IS NOT NULL AND parent_bsuid IS NOT NULL AND phone IS NOT NULL
 AND octet_length(direct_primary_bsuid)<=255 AND octet_length(parent_bsuid)<=255 AND octet_length(phone)<=50
 AND direct_primary_bsuid=pg_catalog.btrim(direct_primary_bsuid,$3) AND parent_bsuid=pg_catalog.btrim(parent_bsuid,$3)
 AND phone !~ '[^0-9]' AND verified_event_provenance='meta_signed_webhook'
 AND octet_length(verified_event_digest)=64 AND octet_length(selector_body_digest)=64 AND octet_length(semantic_claim_digest)=64 AND octet_length(member_digest)=64
 AND verified_event_digest ~ '^[0-9a-f]{64}$' AND selector_body_digest ~ '^[0-9a-f]{64}$' AND semantic_claim_digest ~ '^[0-9a-f]{64}$'
 AND member_digest ~ '^[0-9a-f]{64}$' AND member_count>=0 AND member_count<=4294967295 AND version>0
 AND ((supported AND direct_primary_bsuid<>'' AND principal_generation>0) OR (NOT supported AND principal_generation=0)),false) AS shape_ok
 FROM h),
 classified AS MATERIALIZED (SELECT shaped.*, CASE WHEN NOT shape_ok THEN 'unknown'
 WHEN direct_primary_bsuid<>'' THEN 'direct'
 WHEN octet_length(phone) BETWEEN 7 AND 15 THEN 'phone'
 WHEN parent_bsuid<>'' THEN 'parent_only' ELSE 'no_address' END AS sender_kind FROM shaped),
 member_totals AS MATERIALIZED (SELECT hold_id,count(*) AS actual,
 bool_or(contact_id IS NULL OR contact_id='00000000-0000-0000-0000-000000000000'::uuid OR selector_reasons IS NULL OR selector_reasons<1 OR selector_reasons>7) AS invalid_selector FROM m GROUP BY hold_id),
 duplicate_member_holds AS MATERIALIZED (SELECT hold_id FROM m GROUP BY hold_id,contact_id HAVING count(*)>1),
 direct_units AS MATERIALIZED (SELECT onboarding_cycle,direct_primary_bsuid,
 count(DISTINCT NULLIF(phone,'')) AS phones,count(DISTINCT NULLIF(parent_bsuid,'')) AS parents
 FROM classified WHERE sender_kind='direct' GROUP BY onboarding_cycle,direct_primary_bsuid),
 phone_units AS MATERIALIZED (SELECT onboarding_cycle,phone FROM classified WHERE sender_kind='phone' GROUP BY onboarding_cycle,phone),
 direct_phones AS MATERIALIZED (SELECT onboarding_cycle,phone,count(DISTINCT direct_primary_bsuid) AS principals FROM classified WHERE sender_kind='direct' AND phone<>'' GROUP BY onboarding_cycle,phone),
 receipt_shapes AS MATERIALIZED (SELECT r.*,
 matched_hold.id IS NOT NULL AS linked,COALESCE(matched_hold.shape_ok,false) AS hold_shape_ok,
 COALESCE(c.id IS NOT NULL AND c.channel='whatsapp' AND c.provider='meta'
 AND c.external_account_id='legacy-account:'||$2::text AND c.legacy_account_id=$2::text AND c.deleted_at IS NULL,false) AS exact_account,
 COALESCE(r.protocol='whatsapp_identity_review_v1' AND r.event_type='review_pending',false) AS protocol_ok,
 COALESCE(r.status IN ('pending','processing','processed','ignored','failed'),false) AS status_ok
 FROM r LEFT JOIN classified matched_hold ON matched_hold.id=r.review_hold_id LEFT JOIN c ON c.id=r.channel_account_id),
 receipt_classified AS MATERIALIZED (SELECT receipt_shapes.*,
 id IS NOT NULL AND id<>'00000000-0000-0000-0000-000000000000'::uuid AND linked AND exact_account AND protocol_ok AND status_ok AND signature_valid IS TRUE AND deleted_at IS NULL
 AND hold_shape_ok AS copy_shape_ok
 FROM receipt_shapes),
 -- Retained PR226 Query1: only the two selected-scope predicates are added.
 q1 AS MATERIALIZED (
SELECT h.organization_id, h.whats_app_account_id, h.onboarding_cycle, s.onboarding_cycle AS current_cycle,
       count(*) AS holds, sum(h.member_count) AS member_rows
FROM public.whatsapp_identity_review_holds h
JOIN public.whatsapp_coexistence_states s
  ON s.organization_id = h.organization_id AND s.whats_app_account_id = h.whats_app_account_id
WHERE h.organization_id = $1::uuid AND h.whats_app_account_id = $2::uuid
  AND NOT h.supported AND h.disposition = 'open' AND h.member_count > 0
GROUP BY 1, 2, 3, 4),
 q1_public AS (SELECT row_number() OVER(ORDER BY onboarding_cycle) AS ordinal,
 CASE WHEN onboarding_cycle=current_cycle THEN 'current' WHEN onboarding_cycle>0 AND onboarding_cycle<current_cycle THEN 'older' ELSE 'future_invalid' END AS cycle_relation,
 holds,member_rows AS stored_member_rows FROM q1),
 report AS (SELECT pg_catalog.jsonb_build_object(
 'version',1,'purpose','identity_inventory','scope','selected_account','reason_source','derived_shape',
 'derived_reasons_overlap',true,'scope_failure_policy','whole_report_refusal',
 'current_decidability','not_observed','recorded_authentication','stored_shape_not_fresh_verification','complete_visibility',true,
 'shadow_state',CASE WHEN NOT EXISTS(SELECT 1 FROM c) THEN 'absent'
 WHEN EXISTS(SELECT 1 FROM c WHERE channel='whatsapp' AND provider='meta' AND external_account_id='legacy-account:'||$2::text AND legacy_account_id=$2::text AND deleted_at IS NULL) THEN 'live_exact'
 WHEN EXISTS(SELECT 1 FROM c WHERE channel='whatsapp' AND provider='meta' AND external_account_id='legacy-account:'||$2::text AND legacy_account_id=$2::text AND deleted_at IS NOT NULL) THEN 'deleted_exact' ELSE 'contradictory' END,
 'counts',pg_catalog.jsonb_build_object(
 'holds',(SELECT count(*) FROM h),
 'supported',(SELECT count(*) FROM h WHERE supported IS TRUE),
 'unsupported',(SELECT count(*) FROM h WHERE supported IS FALSE),
 'support_unknown',(SELECT count(*) FROM h WHERE supported IS NULL),
 'open',(SELECT count(*) FROM h WHERE disposition='open'),
 'future_routing',(SELECT count(*) FROM h WHERE disposition='future_routing'),
 'superseded_by_latest',(SELECT count(*) FROM h WHERE disposition='superseded_by_latest'),
 'superseded_by_cycle',(SELECT count(*) FROM h WHERE disposition='superseded_by_cycle'),
 'other_disposition',(SELECT count(*) FROM h WHERE disposition IS NULL OR disposition NOT IN ('open','future_routing','superseded_by_latest','superseded_by_cycle')),
 'current_cycle',(SELECT count(*) FROM h WHERE onboarding_cycle=(SELECT onboarding_cycle FROM s)),
 'older_cycle',(SELECT count(*) FROM h WHERE onboarding_cycle>0 AND onboarding_cycle<(SELECT onboarding_cycle FROM s)),
 'future_or_invalid_cycle',(SELECT count(*) FROM h WHERE onboarding_cycle IS NULL OR onboarding_cycle<=0 OR onboarding_cycle>(SELECT onboarding_cycle FROM s)),
 'protocol_v1',(SELECT count(*) FROM h WHERE protocol_version=1),
 'other_protocol',(SELECT count(*) FROM h WHERE protocol_version IS NULL OR protocol_version<>1),
 'recorded_verified_claim_shape',(SELECT count(*) FROM classified WHERE shape_ok),
 'claim_shape_unknown',(SELECT count(*) FROM classified WHERE NOT shape_ok),
 'direct_present',(SELECT count(*) FROM classified WHERE sender_kind='direct'),
 'phone_candidate',(SELECT count(*) FROM classified WHERE sender_kind='phone'),
 'parent_only',(SELECT count(*) FROM classified WHERE sender_kind='parent_only'),
 'no_address_candidate',(SELECT count(*) FROM classified WHERE sender_kind='no_address'),
 'unsupported_with_direct_legacy',(SELECT count(*) FROM classified WHERE shape_ok AND NOT supported AND direct_primary_bsuid<>''),
 'no_member_snapshot',(SELECT count(*) FROM h WHERE member_count=0),
 'old_cycle_open',(SELECT count(*) FROM h WHERE disposition='open' AND onboarding_cycle>0 AND onboarding_cycle<(SELECT onboarding_cycle FROM s)),
 'direct_sender_units',(SELECT count(*) FROM direct_units),
 'phone_sender_units',(SELECT count(*) FROM phone_units),
 'unknown_sender_hold_incidences',(SELECT count(*) FROM classified WHERE sender_kind IN ('unknown','parent_only','no_address')),
 'member_rows',(SELECT count(*) FROM m),
 'member_selector_invalid',(SELECT count(*) FROM m WHERE contact_id IS NULL OR contact_id='00000000-0000-0000-0000-000000000000'::uuid OR selector_reasons IS NULL OR selector_reasons<1 OR selector_reasons>7),
 'member_duplicate_rows',(SELECT count(*) FROM m)-(SELECT count(*) FROM (SELECT hold_id,contact_id FROM m GROUP BY hold_id,contact_id) unique_members),
 'member_count_mismatch_holds',(SELECT count(*) FROM h LEFT JOIN member_totals m ON m.hold_id=h.id WHERE h.member_count IS NULL OR h.member_count<>COALESCE(m.actual,0)),
 'receipt_rows',(SELECT count(*) FROM r),
 'receipt_linked_to_hold',(SELECT count(*) FROM receipt_classified WHERE linked),
 'receipt_account_only',(SELECT count(*) FROM receipt_classified WHERE NOT linked),
 'receipt_recorded_copy_shape',(SELECT count(*) FROM receipt_classified WHERE copy_shape_ok),
 'receipt_shape_unknown',(SELECT count(*) FROM receipt_classified WHERE NOT copy_shape_ok),
 'receipt_deleted',(SELECT count(*) FROM receipt_classified WHERE deleted_at IS NOT NULL),
 'receipt_wrong_account',(SELECT count(*) FROM receipt_classified WHERE NOT exact_account),
 'receipt_invalid_protocol',(SELECT count(*) FROM receipt_classified WHERE NOT protocol_ok),
 'receipt_invalid_signature',(SELECT count(*) FROM receipt_classified WHERE signature_valid IS NOT TRUE),
 'receipt_invalid_status',(SELECT count(*) FROM receipt_classified WHERE NOT status_ok),
 'receipt_duplicate_rows',(SELECT count(*) FROM r)-(SELECT count(DISTINCT id) FROM r)),
 'overlapping_histograms',pg_catalog.jsonb_build_object(
 'direct_units_with_multiple_phones',(SELECT count(*) FROM direct_units WHERE phones>1),
 'direct_units_with_multiple_parents',(SELECT count(*) FROM direct_units WHERE parents>1),
 'phone_groups_with_multiple_direct_units',(SELECT count(*) FROM direct_phones WHERE principals>1),
 'phone_only_units_also_used_by_direct',(SELECT count(*) FROM phone_units p WHERE EXISTS(SELECT 1 FROM direct_phones d WHERE d.onboarding_cycle=p.onboarding_cycle AND d.phone=p.phone))),
 'derived_reasons',pg_catalog.jsonb_build_object(
 'unsupported_no_direct',(SELECT count(*) FROM classified WHERE shape_ok AND NOT supported AND direct_primary_bsuid=''),
 'unsupported_with_direct_legacy',(SELECT count(*) FROM classified WHERE shape_ok AND NOT supported AND direct_primary_bsuid<>''),
 'supported_current_claim',(SELECT count(*) FROM classified WHERE shape_ok AND supported AND onboarding_cycle=(SELECT onboarding_cycle FROM s)),
 'old_cycle_open',(SELECT count(*) FROM h WHERE disposition='open' AND onboarding_cycle>0 AND onboarding_cycle<(SELECT onboarding_cycle FROM s)),
 'no_member_snapshot',(SELECT count(*) FROM h WHERE member_count=0),
 'member_count_or_selector_shape_inconsistent',(SELECT count(*) FROM h LEFT JOIN member_totals mt ON mt.hold_id=h.id WHERE h.member_count IS NULL OR h.member_count<>COALESCE(mt.actual,0)
 OR mt.invalid_selector OR h.id IN (SELECT hold_id FROM duplicate_member_holds)),
 'claim_provenance_unknown',(SELECT count(*) FROM classified WHERE NOT shape_ok)),
 'query_one',(SELECT COALESCE(jsonb_agg(to_jsonb(q1_public) ORDER BY ordinal),'[]'::jsonb) FROM q1_public)
 ) AS value)
 SELECT CASE WHEN octet_length(value::text)<=65536 THEN value::text END FROM report
`
