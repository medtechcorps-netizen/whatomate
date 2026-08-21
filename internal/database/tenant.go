package database

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	tenantSetting                    = "app.current_organization_id"
	tenantPolicyFingerprintSignature = "public.rereply_tenant_policy_fingerprint()"
	// Keep version 5 for binary rollback compatibility. Additional optional
	// resolvers are verified by name so an older binary can still roll back
	// after the migration without rejecting the unchanged core contract.
	tenantRLSRoutingVersion = 5
)

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
var DirectTenantTables = []string{
	"agent_transfers",
	"ai_contexts",
	"audit_logs",
	"automation_dispatch_states",
	"automation_event_receipts",
	"automation_execution_steps",
	"automation_executions",
	"automation_policies",
	"automation_policy_activations",
	"automation_policy_versions",
	"availability_rules",
	"billing_accounts",
	"billing_usage_rollups",
	"booking_events",
	"booking_resources",
	"booking_service_resources",
	"booking_services",
	"bookings",
	"breach_incidents",
	"bulk_message_campaigns",
	"canned_responses",
	"call_logs",
	"call_permissions",
	"call_transfers",
	"catalog_products",
	"catalogs",
	"channel_accounts",
	"channel_credentials",
	"chatbot_flows",
	"chatbot_sessions",
	"chatbot_settings",
	"commerce_invoices",
	"consent_events",
	"consent_states",
	"contact_channel_preferences",
	"contact_identities",
	"contact_packages",
	"contacts",
	"conversation_notes",
	"conversation_participants",
	"conversation_reads",
	"copilot_feedback",
	"copilot_runs",
	"copilot_settings",
	"credit_balances",
	"credit_ledger_entries",
	"customer_activity_events",
	"crm_leads",
	"crm_pipeline_stages",
	"crm_pipelines",
	"crm_stage_history",
	"entitlement_overrides",
	"follow_up_tasks",
	"google_search_console_properties",
	"inbound_events",
	"inbox_conversations",
	"invoice_lines",
	"invoices",
	"ivr_flows",
	"keyword_rules",
	"legal_holds",
	"message_events",
	"message_parts",
	"messages",
	"meta_analytics_snapshots",
	"meta_instagram_data_deletion_events",
	"notification_rules",
	"organization_onboardings",
	"outbox_events",
	"outbox_jobs",
	"package_definitions",
	"package_entitlements",
	"payment_intents",
	"payment_provider_accounts",
	"payment_transactions",
	"payment_webhook_events",
	"privacy_jobs",
	"privacy_request_events",
	"privacy_requests",
	"provider_integrations",
	"provisioning_runs",
	"recovery_checkpoints",
	"resource_time_off",
	"retention_policies",
	"scheduled_jobs",
	"subscriptions",
	"support_access_grants",
	"support_cases",
	"tags",
	"templates",
	"threads_platform_bindings",
	"usage_events",
	"user_availability_logs",
	"webhooks",
	"whatsapp_accounts",
	"whatsapp_flows",
	"widgets",
	"workspace_template_applications",
	"workspace_template_resource_maps",
}

// DirectTenantTableExemptions lists migrated models that carry an
// organization_id but must be queried before a tenant transaction exists. The
// coverage test requires every new organization-scoped migration to be either
// protected above or explicitly reviewed here with a reason.
var DirectTenantTableExemptions = map[string]string{
	"api_keys":               "API key lookup authenticates the request before tenant context exists",
	"billing_webhook_events": "billing webhook ingestion resolves the tenant from a provider event",
	"custom_actions":         "redirect-token lookup occurs before tenant context exists",
	"custom_roles":           "role lookup is required to authorize and establish tenant context",
	"sso_providers":          "SSO discovery occurs before tenant context exists",
	"teams":                  "team membership participates in authorization before tenant context exists",
	"user_organizations":     "membership lookup establishes tenant context",
	"users":                  "user authentication occurs before tenant context exists",
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
	return withTenantTransaction(db, organizationID, nil, fn)
}

// WithTenantReadCommitted executes fn with tenant RLS context and explicitly
// pins PostgreSQL READ COMMITTED isolation. Use it for row-lock serialization
// protocols whose later statements must observe a policy writer that committed
// while the transaction was waiting for a lock.
func WithTenantReadCommitted(
	db *gorm.DB,
	organizationID uuid.UUID,
	fn func(*gorm.DB) error,
) error {
	return withTenantTransaction(
		db,
		organizationID,
		&sql.TxOptions{Isolation: sql.LevelReadCommitted},
		fn,
	)
}

func withTenantTransaction(
	db *gorm.DB,
	organizationID uuid.UUID,
	options *sql.TxOptions,
	fn func(*gorm.DB) error,
) error {
	if db == nil {
		return errors.New("database connection is required")
	}
	if organizationID == uuid.Nil {
		return ErrMissingTenant
	}
	if fn == nil {
		return errors.New("tenant transaction callback is required")
	}

	run := func(tx *gorm.DB) error {
		if err := SetTenantContext(tx, organizationID); err != nil {
			return err
		}
		return fn(tx)
	}
	if options == nil {
		return db.Transaction(run)
	}
	return db.Transaction(run, options)
}

// ApplyTenantRLS installs fail-closed row-security policies for the CRM tables.
// It must run with the migration/table-owner connection, never the runtime
// application connection. runtimeRole must already exist and must not be a
// superuser, BYPASSRLS, REPLICATION, or a direct/indirect member of any
// migration, privileged, or protected-table-owner role.
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
		Exists      bool
		Superuser   bool
		BypassRLS   bool
		Replication bool
	}
	if err := db.Raw(`
		SELECT
			COUNT(*) = 1 AS exists,
			COALESCE(bool_or(rolsuper), false) AS superuser,
			COALESCE(bool_or(rolbypassrls), false) AS bypass_rls,
			COALESCE(bool_or(rolreplication), false) AS replication
		FROM pg_catalog.pg_roles
		WHERE rolname = ?
	`, runtimeRole).Scan(&roleState).Error; err != nil {
		return fmt.Errorf("inspect runtime role: %w", err)
	}
	if !roleState.Exists {
		return fmt.Errorf("runtime role %q does not exist", runtimeRole)
	}
	if roleState.Superuser || roleState.BypassRLS || roleState.Replication {
		return fmt.Errorf(
			"runtime role %q must be NOSUPERUSER, NOBYPASSRLS, and NOREPLICATION",
			runtimeRole,
		)
	}

	var migrationRoleOID int64
	if err := db.Raw(`
		SELECT oid::bigint
		FROM pg_catalog.pg_roles
		WHERE rolname = ?
	`, migrationRole).Scan(&migrationRoleOID).Error; err != nil {
		return fmt.Errorf("inspect migration role: %w", err)
	}
	if migrationRoleOID == 0 {
		return fmt.Errorf("migration role %q does not exist", migrationRole)
	}
	runtimeIsMigrationMember, err := roleHasMembership(
		db,
		runtimeRole,
		migrationRoleOID,
	)
	if err != nil {
		return fmt.Errorf("inspect runtime role membership: %w", err)
	}
	if runtimeIsMigrationMember {
		return fmt.Errorf(
			"runtime role %q must not be a member of migration role %q",
			runtimeRole,
			migrationRole,
		)
	}
	privilegedRoles, err := privilegedRoleMemberships(db, runtimeRole)
	if err != nil {
		return fmt.Errorf("inspect runtime membership in privileged roles: %w", err)
	}
	if len(privilegedRoles) > 0 {
		return fmt.Errorf(
			"runtime role %q must not be a member of privileged role(s): %s",
			runtimeRole,
			strings.Join(privilegedRoles, ", "),
		)
	}
	installedTenantTables, err := existingProtectedTenantTables(db)
	if err != nil {
		return fmt.Errorf("inspect protected tenant tables: %w", err)
	}
	tableOwnerMemberships, err := protectedTableOwnerMemberships(
		db,
		runtimeRole,
		installedTenantTables,
	)
	if err != nil {
		return fmt.Errorf("inspect runtime membership in protected table owners: %w", err)
	}
	if len(tableOwnerMemberships) > 0 {
		return fmt.Errorf(
			"runtime role %q must not be a member of protected table owner role(s): %s",
			runtimeRole,
			strings.Join(tableOwnerMemberships, ", "),
		)
	}
	installedTenantTableSet := make(map[string]struct{}, len(installedTenantTables))
	for _, table := range installedTenantTables {
		installedTenantTableSet[table] = struct{}{}
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
			if _, exists := installedTenantTableSet[table]; !exists {
				continue
			}
			if err := installPolicy(tx, table, tenantExpr, runtime, migrator); err != nil {
				return err
			}
		}
		for table, expression := range RelatedTenantTables {
			if _, exists := installedTenantTableSet[table]; !exists {
				continue
			}
			if err := installPolicy(tx, table, expression, runtime, migrator); err != nil {
				return err
			}
		}
		if err := installTenantPolicyFingerprint(
			tx,
			installedTenantTables,
			runtime,
		); err != nil {
			return err
		}

		return installRoutingFunctions(tx, runtime)
	}); err != nil {
		return fmt.Errorf("apply tenant RLS: %w", err)
	}

	return nil
}

// VerifyTenantRLS fails startup unless the immutable role that authenticated
// the PostgreSQL backend, the session role, and the current role are all the
// dedicated runtimeRole. It also rejects bypass-capable roles, direct/indirect
// membership in any policy, privileged, fingerprint-owner, or table-owner
// authority, stale tenant state, and incomplete policy installation. This
// prevents a configuration mistake from silently turning database isolation
// off.
func VerifyTenantRLS(db *gorm.DB, runtimeRole string) error {
	if db == nil {
		return errors.New("database connection is required")
	}
	if err := validateIdentifier(runtimeRole); err != nil {
		return fmt.Errorf("invalid runtime role: %w", err)
	}

	var identity struct {
		CurrentRole          string `gorm:"column:current_role"`
		SessionRole          string `gorm:"column:session_role"`
		AuthenticatedRole    string `gorm:"column:authenticated_role"`
		AuthenticatedRoleOID int64  `gorm:"column:authenticated_role_oid"`
		RuntimeRoleOID       int64  `gorm:"column:runtime_role_oid"`
		BackendRows          int64  `gorm:"column:backend_rows"`
	}
	if err := db.Raw(`
		WITH backend_identity AS (
			SELECT
				activity.usesysid::bigint AS authenticated_role_oid,
				activity.usename::text AS authenticated_role
			FROM pg_catalog.pg_stat_activity AS activity
			WHERE activity.pid = pg_catalog.pg_backend_pid()
		), runtime_identity AS (
			SELECT runtime_role.oid::bigint AS runtime_role_oid
			FROM pg_catalog.pg_roles AS runtime_role
			WHERE runtime_role.rolname = ?
		)
		SELECT
			current_user::text AS current_role,
			session_user::text AS session_role,
			COALESCE((SELECT authenticated_role FROM backend_identity), '')
				AS authenticated_role,
			COALESCE((SELECT authenticated_role_oid FROM backend_identity), 0)
				AS authenticated_role_oid,
			COALESCE((SELECT runtime_role_oid FROM runtime_identity), 0)
				AS runtime_role_oid,
			(SELECT COUNT(*)::bigint FROM backend_identity) AS backend_rows
	`, runtimeRole).Scan(&identity).Error; err != nil {
		return fmt.Errorf("read database roles: %w", err)
	}
	if identity.BackendRows != 1 ||
		identity.AuthenticatedRoleOID == 0 ||
		identity.AuthenticatedRole == "" {
		return errors.New("could not prove the authenticated PostgreSQL backend role")
	}
	if identity.RuntimeRoleOID == 0 {
		return fmt.Errorf("runtime role %q does not exist", runtimeRole)
	}
	if identity.AuthenticatedRole != runtimeRole ||
		identity.AuthenticatedRoleOID != identity.RuntimeRoleOID {
		return fmt.Errorf(
			"database backend authenticates as role %q; RLS runtime role must be %q",
			identity.AuthenticatedRole,
			runtimeRole,
		)
	}
	if identity.CurrentRole != runtimeRole {
		return fmt.Errorf(
			"database connection uses role %q; RLS runtime role must be %q",
			identity.CurrentRole,
			runtimeRole,
		)
	}
	if identity.SessionRole != runtimeRole {
		return fmt.Errorf(
			"database connection authenticates as session role %q; RLS runtime role must be %q",
			identity.SessionRole,
			runtimeRole,
		)
	}
	currentRole := identity.CurrentRole

	var roleState struct {
		Superuser   bool
		BypassRLS   bool
		Replication bool
	}
	if err := db.Raw(`
		SELECT
			rolsuper AS superuser,
			rolbypassrls AS bypass_rls,
			rolreplication AS replication
		FROM pg_catalog.pg_roles
		WHERE rolname = current_user
	`).Scan(&roleState).Error; err != nil {
		return fmt.Errorf("inspect current database role: %w", err)
	}
	if roleState.Superuser || roleState.BypassRLS || roleState.Replication {
		return fmt.Errorf(
			"runtime role %q must be NOSUPERUSER, NOBYPASSRLS, and NOREPLICATION",
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
	organizationsExist, err := publicTableExists(db, "organizations")
	if err != nil {
		return fmt.Errorf("inspect organizations table: %w", err)
	}
	organizationsHaveReseller := false
	if organizationsExist {
		organizationsHaveReseller, err = publicColumnExists(
			db,
			"organizations",
			"reseller_id",
		)
		if err != nil {
			return fmt.Errorf("inspect organizations reseller column: %w", err)
		}
	}
	if organizationsHaveReseller {
		var unassignedOrganizations int64
		if err := db.Table("public.organizations").
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

	existingTenantTables, err := existingProtectedTenantTables(db)
	if err != nil {
		return fmt.Errorf("inspect protected tenant tables: %w", err)
	}
	permissivePolicyMembershipByRoleOID := make(map[int64]bool)
	for _, table := range existingTenantTables {
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

		var permissivePolicyRoles []struct {
			PolicyName string `gorm:"column:policy_name"`
			OID        int64  `gorm:"column:role_oid"`
			RoleName   string `gorm:"column:role_name"`
		}
		if err := db.Raw(`
			SELECT
				policy.polname AS policy_name,
				policy_role.role_oid::bigint AS role_oid,
				COALESCE(policy_grantee.rolname, 'PUBLIC') AS role_name
			FROM pg_catalog.pg_policy AS policy
			JOIN pg_catalog.pg_class AS policy_table
			  ON policy_table.oid = policy.polrelid
			JOIN pg_catalog.pg_namespace AS policy_schema
			  ON policy_schema.oid = policy_table.relnamespace
			CROSS JOIN LATERAL unnest(policy.polroles) AS policy_role(role_oid)
			LEFT JOIN pg_catalog.pg_roles AS policy_grantee
			  ON policy_grantee.oid = policy_role.role_oid
			WHERE policy_schema.nspname = 'public'
			  AND policy_table.relname = ?
			  AND policy.polpermissive
			  AND policy.polname <> 'rereply_tenant_isolation'
		`, table).Scan(&permissivePolicyRoles).Error; err != nil {
			return fmt.Errorf("inspect permissive RLS policies on %s: %w", table, err)
		}
		for _, policyRole := range permissivePolicyRoles {
			if policyRole.OID == 0 {
				return fmt.Errorf(
					"permissive RLS policy %q on table %q must not apply to PUBLIC",
					policyRole.PolicyName,
					table,
				)
			}
			isMember, inspected := permissivePolicyMembershipByRoleOID[policyRole.OID]
			if !inspected {
				var err error
				isMember, err = roleHasMembership(db, currentRole, policyRole.OID)
				if err != nil {
					return fmt.Errorf(
						"inspect runtime membership in role %q targeted by permissive RLS policy %q on %s: %w",
						policyRole.RoleName,
						policyRole.PolicyName,
						table,
						err,
					)
				}
				permissivePolicyMembershipByRoleOID[policyRole.OID] = isMember
			}
			if isMember {
				return fmt.Errorf(
					"runtime role %q must not be a member of role %q targeted by permissive RLS policy %q on table %q",
					currentRole,
					policyRole.RoleName,
					policyRole.PolicyName,
					table,
				)
			}
		}
	}
	if err := verifyTenantPolicyFingerprint(db, existingTenantTables, currentRole); err != nil {
		return err
	}
	tableOwnerMemberships, err := protectedTableOwnerMemberships(
		db,
		currentRole,
		existingTenantTables,
	)
	if err != nil {
		return fmt.Errorf("inspect runtime membership in protected table owners: %w", err)
	}
	if len(tableOwnerMemberships) > 0 {
		return fmt.Errorf(
			"runtime role %q must not be a member of protected table owner role(s): %s",
			currentRole,
			strings.Join(tableOwnerMemberships, ", "),
		)
	}
	privilegedRoles, err := privilegedRoleMemberships(db, currentRole)
	if err != nil {
		return fmt.Errorf("inspect runtime membership in privileged roles: %w", err)
	}
	if len(privilegedRoles) > 0 {
		return fmt.Errorf(
			"runtime role %q must not be a member of privileged role(s): %s",
			currentRole,
			strings.Join(privilegedRoles, ", "),
		)
	}

	var whatsappPhoneRoutingIndexValid bool
	if err := db.Raw(`
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_index AS index_state
			JOIN pg_catalog.pg_class AS index_relation
			  ON index_relation.oid = index_state.indexrelid
			JOIN pg_catalog.pg_class AS table_relation
			  ON table_relation.oid = index_state.indrelid
			JOIN pg_catalog.pg_namespace AS table_namespace
			  ON table_namespace.oid = table_relation.relnamespace
			WHERE table_namespace.nspname = 'public'
			  AND table_relation.relname = 'whatsapp_accounts'
			  AND index_relation.relname = 'uq_whatsapp_accounts_live_phone_id'
			  AND index_state.indisunique
			  AND index_state.indisvalid
			  AND index_state.indisready
			  AND index_state.indnkeyatts = 1
			  AND index_state.indnatts = 1
			  AND pg_catalog.pg_get_expr(index_state.indexprs, index_state.indrelid) = 'btrim((phone_id)::text)'
			  AND pg_catalog.pg_get_expr(index_state.indpred, index_state.indrelid) = '(deleted_at IS NULL)'
		)
	`).Scan(&whatsappPhoneRoutingIndexValid).Error; err != nil {
		return fmt.Errorf("inspect WhatsApp Phone ID routing index: %w", err)
	}
	if !whatsappPhoneRoutingIndexValid {
		return errors.New("WhatsApp Phone ID routing index is missing or invalid")
	}

	for _, signature := range []string{
		"public.rereply_resolve_whatsapp_org(text)",
		"public.rereply_resolve_webhook_org(text)",
		"public.rereply_resolve_waba_orgs(text)",
		"public.rereply_resolve_channel_org(uuid)",
		"public.rereply_resolve_meta_channel_org(text,text)",
		"public.rereply_ready_channel_outbox_orgs(uuid,integer,timestamp with time zone)",
		"public.rereply_ready_channel_ai_reply_orgs(uuid,integer,timestamp with time zone)",
		"public.rereply_ready_threads_credential_orgs(uuid,integer,timestamp with time zone)",
		"public.rereply_ready_meta_lifecycle_orgs(uuid,integer,timestamp with time zone)",
		"public.rereply_meta_deauth_targets(text,text)",
		"public.rereply_meta_deauth_target_page(text,text,uuid,integer)",
		"public.rereply_rls_routing_version()",
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
	var routingVersion int
	if err := db.Raw(
		"SELECT public.rereply_rls_routing_version()",
	).Scan(&routingVersion).Error; err != nil {
		return fmt.Errorf("read tenant RLS routing version: %w", err)
	}
	if routingVersion != tenantRLSRoutingVersion {
		return fmt.Errorf(
			"tenant RLS routing version is %d; expected %d (run rereply rls-migrate before deployment)",
			routingVersion,
			tenantRLSRoutingVersion,
		)
	}

	return nil
}

func roleHasMembership(db *gorm.DB, memberRole string, targetRoleOID int64) (bool, error) {
	var isMember bool
	if err := db.Raw(`
		SELECT pg_catalog.pg_has_role(
			CAST(? AS name),
			CAST(? AS oid),
			'MEMBER'
		)
	`, memberRole, targetRoleOID).Scan(&isMember).Error; err != nil {
		return false, err
	}
	return isMember, nil
}

func privilegedRoleMemberships(db *gorm.DB, memberRole string) ([]string, error) {
	var roles []string
	if err := db.Raw(`
		SELECT privileged_role.rolname
		FROM pg_catalog.pg_roles AS privileged_role
		WHERE (
			privileged_role.rolsuper
			OR privileged_role.rolbypassrls
			OR privileged_role.rolreplication
		)
		  AND pg_catalog.pg_has_role(
			CAST(? AS name),
			privileged_role.oid,
			'MEMBER'
		  )
		ORDER BY privileged_role.rolname
	`, memberRole).Scan(&roles).Error; err != nil {
		return nil, err
	}
	return roles, nil
}

type databaseRoleReference struct {
	OID  int64  `gorm:"column:role_oid"`
	Name string `gorm:"column:role_name"`
}

func roleMemberships(
	db *gorm.DB,
	memberRole string,
	targetRoles []databaseRoleReference,
) ([]string, error) {
	memberships := make([]string, 0, len(targetRoles))
	for _, targetRole := range targetRoles {
		isMember, err := roleHasMembership(db, memberRole, targetRole.OID)
		if err != nil {
			return nil, fmt.Errorf("inspect membership in role %q: %w", targetRole.Name, err)
		}
		if isMember {
			memberships = append(memberships, targetRole.Name)
		}
	}
	return memberships, nil
}

func protectedTenantTableNames() []string {
	unique := make(map[string]struct{}, len(DirectTenantTables)+len(RelatedTenantTables))
	for _, table := range DirectTenantTables {
		unique[table] = struct{}{}
	}
	for table := range RelatedTenantTables {
		unique[table] = struct{}{}
	}
	tables := make([]string, 0, len(unique))
	for table := range unique {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	return tables
}

func existingProtectedTenantTables(db *gorm.DB) ([]string, error) {
	tables := protectedTenantTableNames()
	existing := make([]string, 0, len(tables))
	for _, table := range tables {
		exists, err := publicTableExists(db, table)
		if err != nil {
			return nil, fmt.Errorf("inspect public.%s: %w", table, err)
		}
		if exists {
			existing = append(existing, table)
		}
	}
	return existing, nil
}

func protectedTableOwnerMemberships(
	db *gorm.DB,
	memberRole string,
	tables []string,
) ([]string, error) {
	if len(tables) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(tables))
	arguments := make([]any, len(tables))
	for index, table := range tables {
		if err := validateIdentifier(table); err != nil {
			return nil, err
		}
		placeholders[index] = "?"
		arguments[index] = table
	}
	query := fmt.Sprintf(`
		SELECT DISTINCT
			table_owner.oid::bigint AS role_oid,
			table_owner.rolname AS role_name
		FROM pg_catalog.pg_class AS protected_table
		JOIN pg_catalog.pg_namespace AS table_schema
		  ON table_schema.oid = protected_table.relnamespace
		JOIN pg_catalog.pg_roles AS table_owner
		  ON table_owner.oid = protected_table.relowner
		WHERE table_schema.nspname = 'public'
		  AND protected_table.relname IN (%s)
		  AND protected_table.relkind = 'r'
		ORDER BY table_owner.rolname
	`, strings.Join(placeholders, ", "))
	var owners []databaseRoleReference
	if err := db.Raw(query, arguments...).Scan(&owners).Error; err != nil {
		return nil, err
	}
	return roleMemberships(db, memberRole, owners)
}

func publicTableExists(db *gorm.DB, table string) (bool, error) {
	if err := validateIdentifier(table); err != nil {
		return false, err
	}
	var relationState struct {
		Kind               string `gorm:"column:relation_kind"`
		ParticipatesInTree bool   `gorm:"column:participates_in_inheritance"`
	}
	if err := db.Raw(`
		WITH protected_relation AS (
			SELECT relation.oid, relation.relkind
			FROM pg_catalog.pg_class AS relation
			JOIN pg_catalog.pg_namespace AS schema
			  ON schema.oid = relation.relnamespace
			WHERE schema.nspname = 'public'
			  AND relation.relname = ?
		)
		SELECT
			COALESCE((SELECT relkind::text FROM protected_relation), '')
				AS relation_kind,
			EXISTS (
				SELECT 1
				FROM pg_catalog.pg_inherits AS inheritance
				JOIN protected_relation
				  ON protected_relation.oid = inheritance.inhparent
				  OR protected_relation.oid = inheritance.inhrelid
			) AS participates_in_inheritance
	`, table).Scan(&relationState).Error; err != nil {
		return false, err
	}
	switch relationState.Kind {
	case "":
		return false, nil
	case "r":
		if relationState.ParticipatesInTree {
			return false, fmt.Errorf(
				"public.%s participates in PostgreSQL inheritance",
				table,
			)
		}
		return true, nil
	default:
		return false, fmt.Errorf(
			"public.%s has unexpected PostgreSQL relation kind %q",
			table,
			relationState.Kind,
		)
	}
}

func publicColumnExists(db *gorm.DB, table, column string) (bool, error) {
	if err := validateIdentifier(table); err != nil {
		return false, err
	}
	if err := validateIdentifier(column); err != nil {
		return false, err
	}
	tableExists, err := publicTableExists(db, table)
	if err != nil || !tableExists {
		return false, err
	}
	var exists bool
	if err := db.Raw(`
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_attribute AS attribute
			JOIN pg_catalog.pg_class AS relation
			  ON relation.oid = attribute.attrelid
			JOIN pg_catalog.pg_namespace AS schema
			  ON schema.oid = relation.relnamespace
			WHERE schema.nspname = 'public'
			  AND relation.relname = ?
			  AND attribute.attname = ?
			  AND attribute.attnum > 0
			  AND NOT attribute.attisdropped
		)
	`, table, column).Scan(&exists).Error; err != nil {
		return false, err
	}
	return exists, nil
}

type tenantPolicyFingerprintRecord struct {
	Schema          string `gorm:"column:schema_name" json:"schema"`
	Table           string `gorm:"column:table_name" json:"table"`
	Policy          string `gorm:"column:policy_name" json:"policy"`
	Command         string `gorm:"column:command" json:"command"`
	Permissive      bool   `gorm:"column:permissive" json:"permissive"`
	Roles           string `gorm:"column:roles" json:"roles"`
	UsingExpression string `gorm:"column:using_expression" json:"using_expression"`
	CheckExpression string `gorm:"column:check_expression" json:"check_expression"`
}

func tenantPolicyFingerprint(db *gorm.DB, tables []string) (string, error) {
	sortedTables := append([]string(nil), tables...)
	sort.Strings(sortedTables)
	records := make([]tenantPolicyFingerprintRecord, 0, len(sortedTables))
	if len(sortedTables) > 0 {
		placeholders := make([]string, len(sortedTables))
		arguments := make([]any, len(sortedTables))
		for index, table := range sortedTables {
			placeholders[index] = "?"
			arguments[index] = table
		}
		query := fmt.Sprintf(`
			SELECT
				policy_schema.nspname AS schema_name,
				policy_table.relname AS table_name,
				policy.polname AS policy_name,
				policy.polcmd::text AS command,
				policy.polpermissive AS permissive,
				policy.polroles::text AS roles,
				COALESCE(pg_catalog.pg_get_expr(policy.polqual, policy.polrelid, false), '') AS using_expression,
				COALESCE(pg_catalog.pg_get_expr(policy.polwithcheck, policy.polrelid, false), '') AS check_expression
			FROM pg_catalog.pg_policy AS policy
			JOIN pg_catalog.pg_class AS policy_table
			  ON policy_table.oid = policy.polrelid
			JOIN pg_catalog.pg_namespace AS policy_schema
			  ON policy_schema.oid = policy_table.relnamespace
			WHERE policy_schema.nspname = 'public'
			  AND policy.polname = 'rereply_tenant_isolation'
			  AND policy_table.relname IN (%s)
			ORDER BY policy_schema.nspname, policy_table.relname, policy.polname
		`, strings.Join(placeholders, ", "))
		if err := db.Raw(query, arguments...).Scan(&records).Error; err != nil {
			return "", fmt.Errorf("read canonical tenant policies: %w", err)
		}
	}
	if len(records) != len(sortedTables) {
		return "", fmt.Errorf(
			"canonical tenant policy count is %d; expected %d",
			len(records),
			len(sortedTables),
		)
	}

	payload, err := json.Marshal(records)
	if err != nil {
		return "", fmt.Errorf("encode canonical tenant policies: %w", err)
	}
	digest := sha256.Sum256(payload)
	return fmt.Sprintf("%x", digest), nil
}

func installTenantPolicyFingerprint(
	tx *gorm.DB,
	tables []string,
	runtimeRole string,
) error {
	fingerprint, err := tenantPolicyFingerprint(tx, tables)
	if err != nil {
		return fmt.Errorf("compute tenant policy fingerprint: %w", err)
	}
	statements := []string{
		"DROP FUNCTION IF EXISTS public.rereply_tenant_policy_fingerprint()",
		fmt.Sprintf(`CREATE OR REPLACE FUNCTION public.rereply_tenant_policy_fingerprint()
			RETURNS text
			LANGUAGE sql
			IMMUTABLE
			SET search_path = pg_catalog, public
			AS $function$
			  SELECT '%s'::text
			$function$`, fingerprint),
		"REVOKE ALL ON FUNCTION public.rereply_tenant_policy_fingerprint() FROM PUBLIC",
		fmt.Sprintf(
			"GRANT EXECUTE ON FUNCTION public.rereply_tenant_policy_fingerprint() TO %s",
			runtimeRole,
		),
	}
	for _, statement := range statements {
		if err := tx.Exec(statement).Error; err != nil {
			return fmt.Errorf("install tenant policy fingerprint: %w", err)
		}
	}
	return nil
}

func tenantPolicyFingerprintOwner(db *gorm.DB) (databaseRoleReference, error) {
	var owner databaseRoleReference
	if err := db.Raw(`
		SELECT
			function_owner.oid::bigint AS role_oid,
			function_owner.rolname AS role_name
		FROM pg_catalog.pg_proc AS fingerprint_function
		JOIN pg_catalog.pg_namespace AS function_schema
		  ON function_schema.oid = fingerprint_function.pronamespace
		JOIN pg_catalog.pg_roles AS function_owner
		  ON function_owner.oid = fingerprint_function.proowner
		WHERE function_schema.nspname = 'public'
		  AND fingerprint_function.proname = 'rereply_tenant_policy_fingerprint'
		  AND fingerprint_function.pronargs = 0
		  AND fingerprint_function.prokind = 'f'
	`).Scan(&owner).Error; err != nil {
		return databaseRoleReference{}, err
	}
	if owner.OID == 0 || owner.Name == "" {
		return databaseRoleReference{}, errors.New("tenant policy fingerprint owner is missing")
	}
	return owner, nil
}

func verifyTenantPolicyFingerprint(
	db *gorm.DB,
	tables []string,
	runtimeRole string,
) error {
	var exists bool
	if err := db.Raw(
		"SELECT to_regprocedure(?) IS NOT NULL",
		tenantPolicyFingerprintSignature,
	).Scan(&exists).Error; err != nil {
		return fmt.Errorf("inspect tenant policy fingerprint function: %w", err)
	}
	if !exists {
		return errors.New("tenant policy fingerprint is missing (run rereply rls-migrate before deployment)")
	}

	var executable bool
	if err := db.Raw(
		"SELECT has_function_privilege(current_user, ?, 'EXECUTE')",
		tenantPolicyFingerprintSignature,
	).Scan(&executable).Error; err != nil {
		return fmt.Errorf("inspect tenant policy fingerprint privilege: %w", err)
	}
	if !executable {
		return errors.New("runtime role cannot execute the tenant policy fingerprint function")
	}
	owner, err := tenantPolicyFingerprintOwner(db)
	if err != nil {
		return fmt.Errorf("inspect tenant policy fingerprint owner: %w", err)
	}
	runtimeIsOwnerMember, err := roleHasMembership(db, runtimeRole, owner.OID)
	if err != nil {
		return fmt.Errorf("inspect runtime membership in tenant policy fingerprint owner: %w", err)
	}
	if runtimeRole == owner.Name || runtimeIsOwnerMember {
		return fmt.Errorf(
			"runtime role %q must not be a member of tenant policy fingerprint owner role %q",
			runtimeRole,
			owner.Name,
		)
	}

	var expected string
	if err := db.Raw(
		"SELECT public.rereply_tenant_policy_fingerprint()",
	).Scan(&expected).Error; err != nil {
		return fmt.Errorf("read expected tenant policy fingerprint: %w", err)
	}
	actual, err := tenantPolicyFingerprint(db, tables)
	if err != nil {
		return fmt.Errorf("compute current tenant policy fingerprint: %w", err)
	}
	if actual != expected {
		return errors.New("tenant RLS policy fingerprint does not match the applied contract")
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
		// These Instagram SECURITY DEFINER enumerators existed only in an
		// unreleased implementation. Remove any staged copy before installing the
		// retained Messenger/static routing contract.
		"DROP FUNCTION IF EXISTS public.rereply_resolve_managed_instagram_org(text,text,uuid)",
		"DROP FUNCTION IF EXISTS public.rereply_ready_meta_instagram_lifecycle_orgs(uuid,integer)",
		"DROP FUNCTION IF EXISTS public.rereply_ready_meta_instagram_lifecycle_orgs_v2(uuid,integer,uuid,text)",
		"DROP FUNCTION IF EXISTS public.rereply_meta_deauth_target_page_v2(text,text,text,uuid,integer)",
		`CREATE OR REPLACE FUNCTION public.rereply_resolve_whatsapp_org(p_phone_id text)
		 RETURNS uuid
		 LANGUAGE sql
		 STABLE
		 SECURITY DEFINER
		 SET search_path = pg_catalog, public
		 AS $function$
		   SELECT organization_id
		   FROM public.whatsapp_accounts
		   WHERE BTRIM(phone_id) = BTRIM(p_phone_id)
		     AND deleted_at IS NULL
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
		"DROP FUNCTION IF EXISTS public.rereply_resolve_channel_org(text,text,text)",
		`CREATE OR REPLACE FUNCTION public.rereply_resolve_channel_org(
		   p_channel_account_id uuid
		 )
		 RETURNS uuid
		 LANGUAGE sql
		 STABLE
		 SECURITY DEFINER
		 SET search_path = pg_catalog, public
		 AS $function$
		   SELECT organization_id
		   FROM public.channel_accounts
		   WHERE id = p_channel_account_id
		     AND status IN ('pending', 'active', 'degraded')
		     AND deleted_at IS NULL
		 $function$`,
		`CREATE OR REPLACE FUNCTION public.rereply_resolve_meta_channel_org(
		   p_channel text,
		   p_external_account_id text
		 )
		 RETURNS uuid
		 LANGUAGE sql
		 STABLE
		 SECURITY DEFINER
		 SET search_path = pg_catalog, public
		 AS $function$
		   SELECT organization_id
		   FROM public.channel_accounts
		   WHERE channel::text = lower(btrim(p_channel))
		     AND provider = 'relay'
		     AND external_account_id = btrim(p_external_account_id)
		     AND channel::text IN ('messenger', 'instagram')
		     AND status IN ('pending', 'active', 'degraded')
		     AND deleted_at IS NULL
		   LIMIT 1
		 $function$`,
		`CREATE OR REPLACE FUNCTION public.rereply_ready_channel_outbox_orgs(
		   p_after uuid,
		   p_limit integer,
		   p_stale_before timestamptz
		 )
		 RETURNS SETOF uuid
		 LANGUAGE sql
		 SECURITY DEFINER
		 SET search_path = pg_catalog, public
		 AS $function$
		   WITH ready AS (
		     SELECT DISTINCT organization_id
		     FROM public.outbox_jobs
		     WHERE deleted_at IS NULL
		       AND available_at <= clock_timestamp()
		       AND (
		         status IN ('pending', 'retrying')
		         OR (
		           status IN ('processing', 'dispatching')
		           AND (locked_at IS NULL OR locked_at < p_stale_before)
		         )
		       )
		   )
		   SELECT organization_id
		   FROM ready
		   ORDER BY (organization_id > p_after) DESC, organization_id
		   LIMIT LEAST(GREATEST(p_limit, 1), 100)
		 $function$`,
		`CREATE OR REPLACE FUNCTION public.rereply_ready_channel_ai_reply_orgs(
		   p_after uuid,
		   p_limit integer,
		   p_stale_before timestamptz
		 )
		 RETURNS SETOF uuid
		 LANGUAGE sql
		 SECURITY DEFINER
		 SET search_path = pg_catalog, public
		 AS $function$
		   WITH ready AS (
		     SELECT DISTINCT organization_id
		     FROM public.scheduled_jobs
		     WHERE deleted_at IS NULL
		       AND kind = 'channel_ai_reply'
		       AND run_at <= clock_timestamp()
		       AND (
		         status = 'pending'
		         OR (
		         status IN ('processing', 'generating')
		           AND (locked_at IS NULL OR locked_at < p_stale_before)
		         )
		       )
		   )
		   SELECT organization_id
		   FROM ready
		   ORDER BY (organization_id > p_after) DESC, organization_id
		   LIMIT LEAST(GREATEST(p_limit, 1), 100)
		 $function$`,
		`CREATE OR REPLACE FUNCTION public.rereply_ready_threads_credential_orgs(
		   p_after uuid,
		   p_limit integer,
		   p_refresh_before timestamptz
		 )
		 RETURNS SETOF uuid
		 LANGUAGE sql
		 SECURITY DEFINER
		 SET search_path = pg_catalog, public
		 AS $function$
		   WITH ready AS (
		     SELECT DISTINCT credentials.organization_id
		     FROM public.channel_credentials AS credentials
		     JOIN public.channel_accounts AS accounts
		       ON accounts.id = credentials.channel_account_id
		      AND accounts.organization_id = credentials.organization_id
		     WHERE credentials.deleted_at IS NULL
		       AND credentials.kind = 'oauth'
		       AND credentials.status IN ('active', 'expiring')
		       AND credentials.expires_at IS NOT NULL
		       AND credentials.expires_at <= p_refresh_before
		       AND accounts.deleted_at IS NULL
		       AND accounts.channel = 'threads'
		       AND accounts.provider = 'threads'
		       AND accounts.status = 'active'
		   )
		   SELECT organization_id
		   FROM ready
		   ORDER BY (organization_id > p_after) DESC, organization_id
		   LIMIT LEAST(GREATEST(p_limit, 1), 100)
		 $function$`,
		`CREATE OR REPLACE FUNCTION public.rereply_ready_meta_lifecycle_orgs(
		   p_after uuid,
		   p_limit integer,
		   p_due_before timestamptz
		 )
		 RETURNS SETOF uuid
		 LANGUAGE sql
		 SECURITY DEFINER
		 SET search_path = pg_catalog, public
		 AS $function$
		   WITH ready AS (
		     SELECT DISTINCT organization_id
		     FROM public.channel_accounts
		     WHERE deleted_at IS NULL
		       AND channel = 'messenger'
		       AND provider = 'relay'
		       AND status IN ('pending', 'active', 'degraded')
		       AND config ->> 'meta_management_mode' = 'platform_oauth'
		       AND (
		         metadata ->> 'meta_deauthorization_pending_digest' IS NOT NULL
		         OR COALESCE(
		           (metadata ->> 'meta_ownership_checked_at')::timestamptz,
		           '-infinity'::timestamptz
		         ) <= p_due_before
		       )
		   )
		   SELECT organization_id
		   FROM ready
		   WHERE organization_id > p_after
		   ORDER BY organization_id
		   LIMIT LEAST(GREATEST(COALESCE(p_limit, 1), 1), 100)
		 $function$`,
		`CREATE OR REPLACE FUNCTION public.rereply_meta_deauth_target_page(
		   p_app_id text,
		   p_authorizing_user_id text,
		   p_after uuid,
		   p_limit integer
		 )
		 RETURNS TABLE(organization_id uuid, account_id uuid)
		 LANGUAGE sql
		 STABLE
		 SECURITY DEFINER
		 SET search_path = pg_catalog, public
		 AS $function$
		   SELECT accounts.organization_id, accounts.id
		   FROM public.channel_accounts AS accounts
		   WHERE accounts.deleted_at IS NULL
		     AND accounts.channel = 'messenger'
		     AND accounts.provider = 'relay'
		     AND accounts.status IN ('pending', 'active', 'degraded', 'disconnected')
		     AND accounts.config ->> 'meta_management_mode' = 'platform_oauth'
		     AND accounts.metadata ->> 'meta_platform_app_id' = btrim(p_app_id)
		     AND accounts.metadata ->> 'meta_authorizing_user_id' = btrim(p_authorizing_user_id)
		     AND accounts.id > p_after
		   ORDER BY accounts.id
		   LIMIT LEAST(GREATEST(COALESCE(p_limit, 1), 1), 100)
		 $function$`,
		// Temporary PR #52 binary-rollback shim. The legacy signature remains
		// runtime-only and hard-capped by the safe pager; remove it after the
		// production rollback window closes.
		`CREATE OR REPLACE FUNCTION public.rereply_meta_deauth_targets(
		   p_app_id text,
		   p_authorizing_user_id text
		 )
		 RETURNS TABLE(organization_id uuid, account_id uuid)
		 LANGUAGE sql
		 STABLE
		 SECURITY DEFINER
		 SET search_path = pg_catalog, public
		 AS $function$
		   SELECT organization_id, account_id
		   FROM public.rereply_meta_deauth_target_page(
		     p_app_id,
		     p_authorizing_user_id,
		     '00000000-0000-0000-0000-000000000000'::uuid,
		     100
		   )
		 $function$`,
		fmt.Sprintf(
			`CREATE OR REPLACE FUNCTION public.rereply_rls_routing_version()
			 RETURNS integer
			 LANGUAGE sql
			 IMMUTABLE
			 SECURITY DEFINER
			 SET search_path = pg_catalog, public
			 AS $function$
			   SELECT %d
			 $function$`,
			tenantRLSRoutingVersion,
		),
		"REVOKE ALL ON FUNCTION public.rereply_resolve_whatsapp_org(text) FROM PUBLIC",
		"REVOKE ALL ON FUNCTION public.rereply_resolve_webhook_org(text) FROM PUBLIC",
		"REVOKE ALL ON FUNCTION public.rereply_resolve_waba_orgs(text) FROM PUBLIC",
		"REVOKE ALL ON FUNCTION public.rereply_resolve_channel_org(uuid) FROM PUBLIC",
		"REVOKE ALL ON FUNCTION public.rereply_resolve_meta_channel_org(text,text) FROM PUBLIC",
		"REVOKE ALL ON FUNCTION public.rereply_ready_channel_outbox_orgs(uuid,integer,timestamptz) FROM PUBLIC",
		"REVOKE ALL ON FUNCTION public.rereply_ready_channel_ai_reply_orgs(uuid,integer,timestamptz) FROM PUBLIC",
		"REVOKE ALL ON FUNCTION public.rereply_ready_threads_credential_orgs(uuid,integer,timestamptz) FROM PUBLIC",
		"REVOKE ALL ON FUNCTION public.rereply_ready_meta_lifecycle_orgs(uuid,integer,timestamptz) FROM PUBLIC",
		"REVOKE ALL ON FUNCTION public.rereply_meta_deauth_targets(text,text) FROM PUBLIC",
		"REVOKE ALL ON FUNCTION public.rereply_meta_deauth_target_page(text,text,uuid,integer) FROM PUBLIC",
		"REVOKE ALL ON FUNCTION public.rereply_rls_routing_version() FROM PUBLIC",
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION public.rereply_resolve_whatsapp_org(text) TO %s", runtimeRole),
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION public.rereply_resolve_webhook_org(text) TO %s", runtimeRole),
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION public.rereply_resolve_waba_orgs(text) TO %s", runtimeRole),
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION public.rereply_resolve_channel_org(uuid) TO %s", runtimeRole),
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION public.rereply_resolve_meta_channel_org(text,text) TO %s", runtimeRole),
		fmt.Sprintf(
			"GRANT EXECUTE ON FUNCTION public.rereply_ready_channel_outbox_orgs(uuid,integer,timestamptz) TO %s",
			runtimeRole,
		),
		fmt.Sprintf(
			"GRANT EXECUTE ON FUNCTION public.rereply_ready_channel_ai_reply_orgs(uuid,integer,timestamptz) TO %s",
			runtimeRole,
		),
		fmt.Sprintf(
			"GRANT EXECUTE ON FUNCTION public.rereply_ready_threads_credential_orgs(uuid,integer,timestamptz) TO %s",
			runtimeRole,
		),
		fmt.Sprintf(
			"GRANT EXECUTE ON FUNCTION public.rereply_ready_meta_lifecycle_orgs(uuid,integer,timestamptz) TO %s",
			runtimeRole,
		),
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION public.rereply_meta_deauth_targets(text,text) TO %s", runtimeRole),
		fmt.Sprintf("GRANT EXECUTE ON FUNCTION public.rereply_meta_deauth_target_page(text,text,uuid,integer) TO %s", runtimeRole),
		fmt.Sprintf(
			"GRANT EXECUTE ON FUNCTION public.rereply_rls_routing_version() TO %s",
			runtimeRole,
		),
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
		tables, err := existingProtectedTenantTables(tx)
		if err != nil {
			return fmt.Errorf("inspect protected tenant tables for RLS removal: %w", err)
		}
		for _, table := range tables {
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
			"public.rereply_resolve_channel_org(uuid)",
			"public.rereply_resolve_channel_org(text,text,text)",
			"public.rereply_resolve_meta_channel_org(text,text)",
			"public.rereply_ready_channel_outbox_orgs(uuid,integer,timestamptz)",
			"public.rereply_ready_channel_ai_reply_orgs(uuid,integer,timestamptz)",
			"public.rereply_ready_threads_credential_orgs(uuid,integer,timestamptz)",
			"public.rereply_ready_meta_lifecycle_orgs(uuid,integer,timestamptz)",
			"public.rereply_meta_deauth_targets(text,text)",
			"public.rereply_meta_deauth_target_page(text,text,uuid,integer)",
			"public.rereply_tenant_policy_fingerprint()",
			"public.rereply_rls_routing_version()",
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
