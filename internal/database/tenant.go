package database

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

const tenantSetting = "app.current_organization_id"

var (
	// ErrMissingTenant is returned before any database work when a request or
	// background job does not carry an organization identity.
	ErrMissingTenant = errors.New("organization context is required")

	sqlIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// DirectTenantTables contain customer/CRM data with an organization_id column.
//
// Identity and control-plane tables (users, user_organizations, permissions,
// custom_roles, API keys, SSO providers, teams and custom-action redirect
// tokens) intentionally remain protected by the existing authorization layer:
// they are needed to authenticate a request before a tenant transaction exists.
// Calling tables are also kept out until the long-lived WebRTC manager has its
// own tenant-scoped database lifecycle.
var DirectTenantTables = []string{
	"agent_transfers",
	"ai_contexts",
	"audit_logs",
	"bulk_message_campaigns",
	"canned_responses",
	"catalog_products",
	"catalogs",
	"chatbot_flows",
	"chatbot_sessions",
	"chatbot_settings",
	"contacts",
	"conversation_notes",
	"keyword_rules",
	"messages",
	"notification_rules",
	"tags",
	"templates",
	"user_availability_logs",
	"webhooks",
	"whatsapp_accounts",
	"whatsapp_flows",
	"widgets",
}

// RelatedTenantTables do not carry organization_id themselves. Their policies
// derive the tenant through a parent row that is itself protected by RLS.
var RelatedTenantTables = map[string]string{
	"bulk_message_recipients": `EXISTS (
		SELECT 1 FROM public.bulk_message_campaigns parent
		WHERE parent.id = bulk_message_recipients.campaign_id
		  AND parent.organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid
	)`,
	"chatbot_flow_steps": `EXISTS (
		SELECT 1 FROM public.chatbot_flows parent
		WHERE parent.id = chatbot_flow_steps.flow_id
		  AND parent.organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid
	)`,
	"chatbot_session_messages": `EXISTS (
		SELECT 1 FROM public.chatbot_sessions parent
		WHERE parent.id = chatbot_session_messages.session_id
		  AND parent.organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid
	)`,
}

// SetTenantContext binds an organization to the current PostgreSQL
// transaction. It must only be called on a transaction: the third set_config
// argument makes the setting transaction-local so pooled connections cannot
// leak tenant state to a later request.
func SetTenantContext(tx *gorm.DB, organizationID uuid.UUID) error {
	if tx == nil {
		return errors.New("database transaction is required")
	}
	if organizationID == uuid.Nil {
		return ErrMissingTenant
	}
	if err := tx.Exec(
		"SELECT set_config(?, ?, true)",
		tenantSetting,
		organizationID.String(),
	).Error; err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	return nil
}

// WithTenant executes fn in a transaction whose PostgreSQL RLS context is
// restricted to organizationID.
func WithTenant(db *gorm.DB, organizationID uuid.UUID, fn func(*gorm.DB) error) error {
	if db == nil {
		return errors.New("database connection is required")
	}
	if organizationID == uuid.Nil {
		return ErrMissingTenant
	}
	if fn == nil {
		return errors.New("tenant transaction callback is required")
	}

	return db.Transaction(func(tx *gorm.DB) error {
		if err := SetTenantContext(tx, organizationID); err != nil {
			return err
		}
		return fn(tx)
	})
}

// ApplyTenantRLS installs fail-closed row-security policies for the CRM tables.
// It must run with the migration/table-owner connection, never the runtime
// application connection. runtimeRole must already exist and must not be a
// superuser, a BYPASSRLS role, or the migration role.
func ApplyTenantRLS(db *gorm.DB, runtimeRole string) error {
	if db == nil {
		return errors.New("database connection is required")
	}
	if err := validateIdentifier(runtimeRole); err != nil {
		return fmt.Errorf("invalid runtime role: %w", err)
	}

	var migrationRole string
	if err := db.Raw("SELECT current_user").Scan(&migrationRole).Error; err != nil {
		return fmt.Errorf("read migration role: %w", err)
	}
	if err := validateIdentifier(migrationRole); err != nil {
		return fmt.Errorf("invalid migration role: %w", err)
	}
	if strings.EqualFold(runtimeRole, migrationRole) {
		return errors.New("runtime role must be different from the migration role")
	}

	var roleState struct {
		Exists    bool
		Superuser bool
		BypassRLS bool
	}
	if err := db.Raw(`
		SELECT
			COUNT(*) = 1 AS exists,
			COALESCE(bool_or(rolsuper), false) AS superuser,
			COALESCE(bool_or(rolbypassrls), false) AS bypass_rls
		FROM pg_catalog.pg_roles
		WHERE rolname = ?
	`, runtimeRole).Scan(&roleState).Error; err != nil {
		return fmt.Errorf("inspect runtime role: %w", err)
	}
	if !roleState.Exists {
		return fmt.Errorf("runtime role %q does not exist", runtimeRole)
	}
	if roleState.Superuser || roleState.BypassRLS {
		return fmt.Errorf("runtime role %q must be NOSUPERUSER and NOBYPASSRLS", runtimeRole)
	}

	runtime := quoteIdentifier(runtimeRole)
	migrator := quoteIdentifier(migrationRole)

	if err := db.Transaction(func(tx *gorm.DB) error {
		// The runtime process needs ordinary DML rights, while RLS determines
		// which rows are visible. Future tables inherit the same grants.
		if err := tx.Exec(fmt.Sprintf(
			"GRANT USAGE ON SCHEMA public TO %s", runtime,
		)).Error; err != nil {
			return err
		}
		if err := tx.Exec(fmt.Sprintf(
			"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s", runtime,
		)).Error; err != nil {
			return err
		}
		if err := tx.Exec(fmt.Sprintf(
			"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s", runtime,
		)).Error; err != nil {
			return err
		}
		if err := tx.Exec(fmt.Sprintf(
			"ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s",
			migrator, runtime,
		)).Error; err != nil {
			return err
		}
		if err := tx.Exec(fmt.Sprintf(
			"ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO %s",
			migrator, runtime,
		)).Error; err != nil {
			return err
		}

		tenantExpr := "organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid"
		for _, table := range DirectTenantTables {
			if !tx.Migrator().HasTable(table) {
				continue
			}
			if err := installPolicy(tx, table, tenantExpr, runtime, migrator); err != nil {
				return err
			}
		}
		for table, expression := range RelatedTenantTables {
			if !tx.Migrator().HasTable(table) {
				continue
			}
			if err := installPolicy(tx, table, expression, runtime, migrator); err != nil {
				return err
			}
		}

		return installRoutingFunctions(tx, runtime)
	}); err != nil {
		return fmt.Errorf("apply tenant RLS: %w", err)
	}

	return nil
}

// VerifyTenantRLS fails startup when an RLS-enabled server or worker is using
// the owner connection, a bypass-capable role, a pool with stale tenant state,
// or an incomplete policy installation. This prevents a configuration mistake
// from silently turning database isolation off.
func VerifyTenantRLS(db *gorm.DB, runtimeRole string) error {
	if db == nil {
		return errors.New("database connection is required")
	}
	if err := validateIdentifier(runtimeRole); err != nil {
		return fmt.Errorf("invalid runtime role: %w", err)
	}

	var currentRole string
	if err := db.Raw("SELECT current_user").Scan(&currentRole).Error; err != nil {
		return fmt.Errorf("read current database role: %w", err)
	}
	if currentRole != runtimeRole {
		return fmt.Errorf(
			"database connection uses role %q; RLS runtime role must be %q",
			currentRole,
			runtimeRole,
		)
	}

	var roleState struct {
		Superuser bool
		BypassRLS bool
	}
	if err := db.Raw(`
		SELECT rolsuper AS superuser, rolbypassrls AS bypass_rls
		FROM pg_catalog.pg_roles
		WHERE rolname = current_user
	`).Scan(&roleState).Error; err != nil {
		return fmt.Errorf("inspect current database role: %w", err)
	}
	if roleState.Superuser || roleState.BypassRLS {
		return fmt.Errorf(
			"runtime role %q must be NOSUPERUSER and NOBYPASSRLS",
			currentRole,
		)
	}

	var staleTenant bool
	if err := db.Raw(
		"SELECT NULLIF(current_setting(?, true), '') IS NOT NULL",
		tenantSetting,
	).Scan(&staleTenant).Error; err != nil {
		return fmt.Errorf("inspect tenant connection state: %w", err)
	}
	if staleTenant {
		return errors.New("database pool contains a session-level tenant context")
	}

	// Every active organization must belong to a reseller portfolio. This
	// control-plane invariant prevents unscoped customer organizations from
	// appearing after a partial migration or manual database change.
	if db.Migrator().HasTable("organizations") &&
		db.Migrator().HasColumn("organizations", "reseller_id") {
		var unassignedOrganizations int64
		if err := db.Table("organizations").
			Where("deleted_at IS NULL AND reseller_id IS NULL").
			Count(&unassignedOrganizations).Error; err != nil {
			return fmt.Errorf("verify organization reseller ownership: %w", err)
		}
		if unassignedOrganizations > 0 {
			return fmt.Errorf(
				"%d active organization(s) have no reseller assignment",
				unassignedOrganizations,
			)
		}
	}

	tables := append([]string{}, DirectTenantTables...)
	for table := range RelatedTenantTables {
		tables = append(tables, table)
	}
	for _, table := range tables {
		if !db.Migrator().HasTable(table) {
			continue
		}

		var policyState struct {
			RowSecurity      bool
			ForceRowSecurity bool
			TenantPolicy     bool
		}
		if err := db.Raw(`
			SELECT
				c.relrowsecurity AS row_security,
				c.relforcerowsecurity AS force_row_security,
				EXISTS (
					SELECT 1
					FROM pg_catalog.pg_policies p
					WHERE p.schemaname = 'public'
					  AND p.tablename = ?
					  AND p.policyname = 'rereply_tenant_isolation'
					  AND CAST(? AS name) = ANY(p.roles)
				) AS tenant_policy
			FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relname = ?
		`, table, runtimeRole, table).Scan(&policyState).Error; err != nil {
			return fmt.Errorf("inspect RLS policy on %s: %w", table, err)
		}
		if !policyState.RowSecurity || !policyState.ForceRowSecurity || !policyState.TenantPolicy {
			return fmt.Errorf("tenant RLS is incomplete on table %q", table)
		}
	}

	for _, signature := range []string{
		"public.rereply_resolve_whatsapp_org(text)",
		"public.rereply_resolve_webhook_org(text)",
		"public.rereply_resolve_waba_orgs(text)",
	} {
		var allowed bool
		if err := db.Raw(
			"SELECT has_function_privilege(current_user, ?, 'EXECUTE')",
			signature,
		).Scan(&allowed).Error; err != nil {
			return fmt.Errorf("inspect routing function %s: %w", signature, err)
		}
		if !allowed {
			return fmt.Errorf("runtime role cannot execute routing function %s", signature)
		}
	}

	return nil
}

func installPolicy(tx *gorm.DB, table, expression, runtimeRole, migrationRole string) error {
	if err := validateIdentifier(table); err != nil {
		return err
	}
	tableID := quoteIdentifier(table)
	tenantPolicy := quoteIdentifier("rereply_tenant_isolation")
	migrationPolicy := quoteIdentifier("rereply_migration_access")

	statements := []string{
		fmt.Sprintf("ALTER TABLE public.%s ENABLE ROW LEVEL SECURITY", tableID),
		fmt.Sprintf("ALTER TABLE public.%s FORCE ROW LEVEL SECURITY", tableID),
		fmt.Sprintf("DROP POLICY IF EXISTS %s ON public.%s", tenantPolicy, tableID),
		fmt.Sprintf(
			"CREATE POLICY %s ON public.%s TO %s USING (%s) WITH CHECK (%s)",
			tenantPolicy, tableID, runtimeRole, expression, expression,
		),
		fmt.Sprintf("DROP POLICY IF EXISTS %s ON public.%s", migrationPolicy, tableID),
		fmt.Sprintf(
			"CREATE POLICY %s ON public.%s TO %s USING (true) WITH CHECK (true)",
			migrationPolicy, tableID, migrationRole,
		),
	}
	for _, statement := range statements {
		if err := tx.Exec(statement).Error; err != nil {
			return fmt.Errorf("install RLS policy on %s: %w", table, err)
		}
	}
	return nil
}

func installRoutingFunctions(tx *gorm.DB, runtimeRole string) error {
	statements := []string{
		`CREATE OR REPLACE FUNCTION public.rereply_resolve_whatsapp_org(p_phone_id text)
		 RETURNS uuid
		 LANGUAGE sql
		 STABLE
		 SECURITY DEFINER
		 SET search_path = pg_catalog, public
		 AS $function$
		   SELECT organization_id
		   FROM public.whatsapp_accounts
		   WHERE phone_id = p_phone_id AND deleted_at IS NULL
		   LIMIT 1
		 $function$`,
		`CREATE OR REPLACE FUNCTION public.rereply_resolve_webhook_org(p_verify_token text)
		 RETURNS uuid
		 LANGUAGE sql
		 STABLE
		 SECURITY DEFINER
		 SET search_path = pg_catalog, public
		 AS $function$
		   SELECT organization_id
		   FROM public.whatsapp_accounts
		   WHERE webhook_verify_token = p_verify_token AND deleted_at IS NULL
		   LIMIT 1
		 $function$`,
		`CREATE OR REPLACE FUNCTION public.rereply_resolve_waba_orgs(p_business_id text)
		 RETURNS SETOF uuid
		 LANGUAGE sql
		 STABLE
		 SECURITY DEFINER
		 SET search_path = pg_catalog, public
		 AS $function$
		   SELECT DISTINCT organization_id
		   FROM public.whatsapp_accounts
		   WHERE business_id = p_business_id AND deleted_at IS NULL
		 $function$`,
		"REVOKE ALL ON FUNCTION public.rereply_resolve_whatsapp_org(text) FROM PUBLIC",
		"REVOKE ALL ON FUNCTION public.rereply_resolve_webhook_org(text) FROM PUBLIC",
		"REVOKE ALL ON FUNCTION public.rereply_resolve_waba_orgs(text) FROM PUBLIC",
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION public.rereply_resolve_whatsapp_org(text) TO %s", runtimeRole),
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION public.rereply_resolve_webhook_org(text) TO %s", runtimeRole),
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION public.rereply_resolve_waba_orgs(text) TO %s", runtimeRole),
	}
	for _, statement := range statements {
		if err := tx.Exec(statement).Error; err != nil {
			return fmt.Errorf("install webhook tenant resolver: %w", err)
		}
	}
	return nil
}

// RemoveTenantRLS is the operational rollback for a failed staged rollout. It
// removes ReReply's policies and disables RLS without deleting application data.
func RemoveTenantRLS(db *gorm.DB) error {
	if db == nil {
		return errors.New("database connection is required")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		tables := append([]string{}, DirectTenantTables...)
		for table := range RelatedTenantTables {
			tables = append(tables, table)
		}
		for _, table := range tables {
			if !tx.Migrator().HasTable(table) {
				continue
			}
			tableID := quoteIdentifier(table)
			for _, statement := range []string{
				fmt.Sprintf("DROP POLICY IF EXISTS rereply_tenant_isolation ON public.%s", tableID),
				fmt.Sprintf("DROP POLICY IF EXISTS rereply_migration_access ON public.%s", tableID),
				fmt.Sprintf("ALTER TABLE public.%s NO FORCE ROW LEVEL SECURITY", tableID),
				fmt.Sprintf("ALTER TABLE public.%s DISABLE ROW LEVEL SECURITY", tableID),
			} {
				if err := tx.Exec(statement).Error; err != nil {
					return err
				}
			}
		}
		for _, signature := range []string{
			"public.rereply_resolve_whatsapp_org(text)",
			"public.rereply_resolve_webhook_org(text)",
			"public.rereply_resolve_waba_orgs(text)",
		} {
			if err := tx.Exec("DROP FUNCTION IF EXISTS " + signature).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func validateIdentifier(value string) error {
	if !sqlIdentifier.MatchString(value) {
		return fmt.Errorf("%q is not a safe PostgreSQL identifier", value)
	}
	return nil
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
