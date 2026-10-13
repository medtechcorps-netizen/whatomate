package database

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// The seed observer has a closed table set, separate from catalog capture's
// metadata-only global allowlist. Owning a relation alone does not establish
// all-row visibility when FORCE RLS is enabled.
func planSeedTables(ctx context.Context, tx pgx.Tx, tables ...string) error {
	for _, table := range tables {
		if ctx.Err() != nil {
			return errPlanSeedUnavailable
		}
		switch table {
		case "permissions", "custom_roles", "role_permissions", "organizations", "users",
			"resellers", "reseller_members", "plans", "plan_prices", "plan_entitlements":
			var allowed bool
			err := tx.QueryRow(ctx, `SELECT c.relkind='r' AND c.relpersistence='p'
 AND c.relowner=(SELECT oid FROM pg_catalog.pg_roles WHERE rolname=current_user)
 AND pg_catalog.has_table_privilege(current_user,c.oid,'SELECT')
 AND NOT c.relrowsecurity AND NOT c.relforcerowsecurity
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_policy p WHERE p.polrelid=c.oid)
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_inherits i WHERE i.inhparent=c.oid OR i.inhrelid=c.oid)
 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND c.relname=$1`, table).Scan(&allowed)
			if err != nil || !allowed {
				return errPlanSeedUnavailable
			}
		case "widgets":
			if err := planSeedTenantTable(ctx, tx, table); err != nil {
				return err
			}
		default:
			return errPlanSeedUnavailable
		}
	}
	return nil
}

func planSeedTenantTable(ctx context.Context, tx pgx.Tx, table string) error {
	var allowed bool
	var using, check string
	err := tx.QueryRow(ctx, `SELECT c.relkind='r' AND c.relpersistence='p'
 AND c.relowner=(SELECT oid FROM pg_catalog.pg_roles WHERE rolname=current_user)
 AND pg_catalog.has_table_privilege(current_user,c.oid,'SELECT')
 AND c.relrowsecurity AND c.relforcerowsecurity
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_inherits i WHERE i.inhparent=c.oid OR i.inhrelid=c.oid)
 AND (SELECT pg_catalog.count(*) FROM pg_catalog.pg_policy p WHERE p.polrelid=c.oid)=2
 AND EXISTS(SELECT 1 FROM pg_catalog.pg_policy m WHERE m.polrelid=c.oid AND m.polname='rereply_migration_access'
  AND m.polpermissive AND m.polcmd='*' AND m.polroles=ARRAY[c.relowner]::oid[]
  AND pg_catalog.pg_get_expr(m.polqual,m.polrelid,false)='true'
  AND pg_catalog.pg_get_expr(m.polwithcheck,m.polrelid,false)='true')
 AND tenant.polpermissive AND tenant.polcmd='*' AND pg_catalog.array_length(tenant.polroles,1)=1
 AND role.oid<>c.relowner AND NOT role.rolsuper AND NOT role.rolbypassrls AND NOT role.rolcreaterole AND NOT role.rolreplication
 AND NOT pg_catalog.pg_has_role(role.oid,c.relowner,'MEMBER'),
 COALESCE(pg_catalog.pg_get_expr(tenant.polqual,tenant.polrelid,false),''),
 COALESCE(pg_catalog.pg_get_expr(tenant.polwithcheck,tenant.polrelid,false),'')
 FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 JOIN pg_catalog.pg_policy tenant ON tenant.polrelid=c.oid AND tenant.polname='rereply_tenant_isolation'
 JOIN pg_catalog.pg_roles role ON role.oid=tenant.polroles[1]
 WHERE n.nspname='public' AND c.relname=$1`, table).Scan(&allowed, &using, &check)
	if err != nil || !allowed {
		return errPlanSeedUnavailable
	}
	expected, err := canonicalTenantPolicyExpression(table)
	if err != nil {
		return errPlanSeedUnavailable
	}
	u, err := canonicalizeTenantPolicyExpression(using)
	if err != nil {
		return errPlanSeedUnavailable
	}
	c, err := canonicalizeTenantPolicyExpression(check)
	if err != nil || u != expected || c != expected {
		return errPlanSeedUnavailable
	}
	return planSeedPolicyDependencies(ctx, tx, table)
}

// Match the existing complete read-only tenant-policy dependency validator.
// The pgx route cannot borrow a GORM pool while its caller owns the snapshot.
func planSeedPolicyDependencies(ctx context.Context, tx pgx.Tx, table string) error {
	relations, err := canonicalTenantPolicyRelationNames(table)
	if err != nil {
		return errPlanSeedUnavailable
	}
	var policy, procedures, currentSetting, unexpectedProcedures, relationCount, expectedRelations, unexpectedRelations, operators, unexpectedOperators, unexpectedTypes int64
	err = tx.QueryRow(ctx, planSeedPolicyDependencySQL, table, relations).Scan(&policy, &procedures, &currentSetting, &unexpectedProcedures,
		&relationCount, &expectedRelations, &unexpectedRelations, &operators, &unexpectedOperators, &unexpectedTypes)
	if err != nil || policy != 1 || procedures != currentSetting || unexpectedProcedures != 0 || relationCount != int64(len(relations)) ||
		expectedRelations != int64(len(relations)) || unexpectedRelations != 0 || unexpectedOperators != 0 || unexpectedTypes != 0 {
		return errPlanSeedUnavailable
	}
	return nil
}

const planSeedPolicyDependencySQL = `WITH target_policy AS (
 SELECT policy.oid FROM pg_catalog.pg_policy policy
 JOIN pg_catalog.pg_class relation ON relation.oid=policy.polrelid
 JOIN pg_catalog.pg_namespace namespace ON namespace.oid=relation.relnamespace
 WHERE namespace.nspname='public' AND relation.relname=$1 AND policy.polname='rereply_tenant_isolation'
), dependency AS (
 SELECT dependency.* FROM pg_catalog.pg_depend dependency JOIN target_policy ON target_policy.oid=dependency.objid
 WHERE dependency.classid='pg_catalog.pg_policy'::pg_catalog.regclass
)
SELECT (SELECT pg_catalog.count(*) FROM target_policy),
 pg_catalog.count(DISTINCT procedure.oid) FILTER(WHERE dependency.refclassid='pg_catalog.pg_proc'::pg_catalog.regclass),
 pg_catalog.count(DISTINCT procedure.oid) FILTER(WHERE procedure.oid=pg_catalog.to_regprocedure('pg_catalog.current_setting(text,boolean)')),
 pg_catalog.count(DISTINCT procedure.oid) FILTER(WHERE dependency.refclassid='pg_catalog.pg_proc'::pg_catalog.regclass AND procedure.oid<>pg_catalog.to_regprocedure('pg_catalog.current_setting(text,boolean)')),
 pg_catalog.count(DISTINCT relation.oid) FILTER(WHERE dependency.refclassid='pg_catalog.pg_class'::pg_catalog.regclass),
 pg_catalog.count(DISTINCT relation.oid) FILTER(WHERE dependency.refclassid='pg_catalog.pg_class'::pg_catalog.regclass AND relation_namespace.nspname='public' AND relation.relname=ANY($2::text[])),
 pg_catalog.count(DISTINCT relation.oid) FILTER(WHERE dependency.refclassid='pg_catalog.pg_class'::pg_catalog.regclass AND NOT(relation_namespace.nspname='public' AND relation.relname=ANY($2::text[]))),
 pg_catalog.count(DISTINCT operator.oid) FILTER(WHERE dependency.refclassid='pg_catalog.pg_operator'::pg_catalog.regclass),
 pg_catalog.count(DISTINCT operator.oid) FILTER(WHERE dependency.refclassid='pg_catalog.pg_operator'::pg_catalog.regclass AND (operator_namespace.nspname<>'pg_catalog' OR operator.oprname<>'=')),
 pg_catalog.count(DISTINCT referenced_type.oid) FILTER(WHERE dependency.refclassid='pg_catalog.pg_type'::pg_catalog.regclass AND NOT(type_namespace.nspname='pg_catalog' AND referenced_type.typname IN('bool','text','uuid')))
FROM dependency
LEFT JOIN pg_catalog.pg_proc procedure ON dependency.refclassid='pg_catalog.pg_proc'::pg_catalog.regclass AND procedure.oid=dependency.refobjid
LEFT JOIN pg_catalog.pg_class relation ON dependency.refclassid='pg_catalog.pg_class'::pg_catalog.regclass AND relation.oid=dependency.refobjid
LEFT JOIN pg_catalog.pg_namespace relation_namespace ON relation_namespace.oid=relation.relnamespace
LEFT JOIN pg_catalog.pg_operator operator ON dependency.refclassid='pg_catalog.pg_operator'::pg_catalog.regclass AND operator.oid=dependency.refobjid
LEFT JOIN pg_catalog.pg_namespace operator_namespace ON operator_namespace.oid=operator.oprnamespace
LEFT JOIN pg_catalog.pg_type referenced_type ON dependency.refclassid='pg_catalog.pg_type'::pg_catalog.regclass AND referenced_type.oid=dependency.refobjid
LEFT JOIN pg_catalog.pg_namespace type_namespace ON type_namespace.oid=referenced_type.typnamespace`
