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
	tenantSetting                            = "app.current_organization_id"
	tenantPolicyFingerprintSignature         = "public.rereply_tenant_policy_fingerprint()"
	tenantAdditivePolicyFingerprintSignature = "public.rereply_tenant_policy_additive_fingerprint_v1()"
	// Keep version 5 for binary rollback compatibility. Additional optional
	// resolvers are verified by name so an older binary can still roll back
	// after the migration without rejecting the unchanged core contract.
	tenantRLSRoutingVersion         = 5
	tenantRLSAdditiveProfileVersion = 1
)

// tenantRLSAdditiveProfileV1 is deliberately outside the version-5 core
// fingerprint. A prior version-5 binary therefore continues to verify the
// unchanged core after these additive tables are installed, while the current
// binary separately requires this complete, versioned profile and its exact
// fingerprint. Never add a core table here merely to make a mismatch pass.
var tenantRLSAdditiveProfileV1 = []string{
	"whatsapp_coexistence_states",
	"whatsapp_identity_review_holds",
	"whatsapp_identity_review_members",
}

var (
	// ErrMissingTenant is returned before any database work when a request or
	// background job does not carry an organization identity.
	ErrMissingTenant = errors.New("organization context is required")
	// ErrTenantRLSRemovalTransaction rejects nested or stale-snapshot rollback
	// attempts. RLS removal is an operational root transaction whose first
	// visibility boundary must be a READ COMMITTED organizations lock.
	ErrTenantRLSRemovalTransaction = errors.New("tenant RLS removal requires a root READ COMMITTED transaction")

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
	"whatsapp_coexistence_states",
	"whatsapp_flows",
	"whatsapp_identity_review_holds",
	"whatsapp_identity_review_members",
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
		  AND parent.organization_id = NULLIF(pg_catalog.current_setting('app.current_organization_id', true), '')::uuid
	)`,
	"chatbot_flow_steps": `EXISTS (
		SELECT 1 FROM public.chatbot_flows parent
		WHERE parent.id = chatbot_flow_steps.flow_id
		  AND parent.organization_id = NULLIF(pg_catalog.current_setting('app.current_organization_id', true), '')::uuid
	)`,
	"chatbot_session_messages": `EXISTS (
		SELECT 1 FROM public.chatbot_sessions parent
		WHERE parent.id = chatbot_session_messages.session_id
		  AND parent.organization_id = NULLIF(pg_catalog.current_setting('app.current_organization_id', true), '')::uuid
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
		"SELECT pg_catalog.set_config(?, ?, true)",
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

// establishMigrationSchemaTrust closes the public-schema creation boundary
// before privileged preparers can resolve application-schema overloads. Revoking
// CREATE does not remove previously planted functions, so reject the known
// preparatory btrim shadow class by catalog inspection without executing it.
func establishMigrationSchemaTrust(db *gorm.DB, runtimeRole string) error {
	if db == nil {
		return errors.New("database connection is required")
	}
	if err := validateIdentifier(runtimeRole); err != nil {
		return fmt.Errorf("invalid runtime role: %w", err)
	}
	return db.Transaction(func(tx *gorm.DB) error {
		runtimeRoleOID, err := exactDatabaseRoleOID(tx, runtimeRole)
		if err != nil {
			return fmt.Errorf("inspect runtime role before migration schema trust: %w", err)
		}
		if err := revokeRuntimePublicSchemaCreate(tx, runtimeRole, runtimeRoleOID); err != nil {
			return err
		}
		shadowCount, err := publicFunctionNameCount(tx, "btrim")
		if err != nil {
			return fmt.Errorf("inspect pre-existing public.btrim overloads: %w", err)
		}
		if shadowCount != 0 {
			return fmt.Errorf("public schema contains %d pre-existing public.btrim function overload(s)", shadowCount)
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
}

// revokeRuntimePublicSchemaCreate runs inside the caller's transaction so a
// failed postcondition rolls back both revokes. Use it again during policy
// publication even after the coordinator established the preparation boundary.
func revokeRuntimePublicSchemaCreate(db *gorm.DB, runtimeRole string, runtimeRoleOID int64) error {
	if err := validateIdentifier(runtimeRole); err != nil {
		return fmt.Errorf("invalid runtime role: %w", err)
	}
	if runtimeRoleOID <= 0 {
		return errors.New("runtime public-schema authority role binding is missing")
	}
	// PostgreSQL 14 grants CREATE on public to PUBLIC in a fresh database.
	// Runtime principals need USAGE only; retaining CREATE would let them add
	// search-path shadows for policy functions, operators, or relations.
	if err := db.Exec("REVOKE CREATE ON SCHEMA public FROM PUBLIC").Error; err != nil {
		return err
	}
	if err := db.Exec(fmt.Sprintf(
		"REVOKE CREATE ON SCHEMA public FROM %s", quoteIdentifier(runtimeRole),
	)).Error; err != nil {
		return err
	}
	// REVOKE may only warn when the migrator cannot remove another
	// grantor's privilege. Prove PUBLIC and effective (including inherited)
	// runtime authority before permitting any later migration work.
	var schemaAuthority struct {
		NamespaceCount   int64 `gorm:"column:namespace_count"`
		PublicCanCreate  bool  `gorm:"column:public_can_create"`
		RuntimeCanCreate bool  `gorm:"column:runtime_can_create"`
	}
	observation := db.Raw(`
		SELECT pg_catalog.count(*) AS namespace_count,
			COALESCE(pg_catalog.bool_or(EXISTS (
				SELECT 1
				FROM pg_catalog.aclexplode(COALESCE(
					namespace.nspacl,
					pg_catalog.acldefault('n', namespace.nspowner)
				)) AS privilege
				WHERE privilege.grantee = 0
				  AND privilege.privilege_type = 'CREATE'
			)), true) AS public_can_create,
			COALESCE(pg_catalog.bool_or(pg_catalog.has_schema_privilege(
				CAST(? AS pg_catalog.oid), namespace.oid, 'CREATE'
			)), true) AS runtime_can_create
		FROM pg_catalog.pg_namespace AS namespace
		WHERE namespace.nspname = 'public'
	`, runtimeRoleOID).Scan(&schemaAuthority)
	if observation.Error != nil {
		return fmt.Errorf("inspect public schema CREATE revocation: %w", observation.Error)
	}
	if observation.RowsAffected != 1 || schemaAuthority.NamespaceCount != 1 ||
		schemaAuthority.PublicCanCreate || schemaAuthority.RuntimeCanCreate {
		return errors.New("public schema CREATE revocation postcondition failed")
	}
	return verifyNoSettablePublicSchemaCreator(db, runtimeRole, runtimeRoleOID)
}

// verifyNoSettablePublicSchemaCreator binds traversal to the supplied runtime
// identity, never to the privileged migration connection's current_user.
func verifyNoSettablePublicSchemaCreator(db *gorm.DB, runtimeRole string, runtimeRoleOID int64) error {
	if runtimeRoleOID <= 0 {
		return errors.New("settable public-schema authority role binding is missing")
	}
	var settablePublicCreator bool
	if err := db.Raw(`
		WITH RECURSIVE settable_role(role_oid, path, can_set) AS (
			SELECT role.oid, ARRAY[role.oid]::pg_catalog.oid[], true
			FROM pg_catalog.pg_roles AS role
			WHERE role.oid = CAST(? AS pg_catalog.oid)
			UNION ALL
			SELECT membership.roleid,
				pg_catalog.array_append(settable_role.path, membership.roleid),
				settable_role.can_set AND COALESCE(
					(pg_catalog.to_jsonb(membership)->>'set_option')::boolean,
					true
				)
			FROM settable_role
			JOIN pg_catalog.pg_auth_members AS membership
			  ON membership.member = settable_role.role_oid
			WHERE NOT membership.roleid = ANY(settable_role.path)
		)
		SELECT EXISTS (
			SELECT 1
			FROM settable_role
			WHERE can_set
			  AND role_oid <> CAST(? AS pg_catalog.oid)
			  AND pg_catalog.has_schema_privilege(role_oid, 'public', 'CREATE')
		)
	`, runtimeRoleOID, runtimeRoleOID).Scan(&settablePublicCreator).Error; err != nil {
		return fmt.Errorf("inspect settable public-schema creator authority: %w", err)
	}
	if settablePublicCreator {
		return fmt.Errorf("runtime role %q can SET ROLE to a public-schema creator", runtimeRole)
	}
	return nil
}

// ApplyTenantRLS installs fail-closed row-security policies for the CRM tables.
// It must run with the migration/table-owner connection, never the runtime
// application connection. runtimeRole must already exist and must not be a
// superuser, CREATEROLE, BYPASSRLS, REPLICATION, or a direct/indirect member of any
// migration, privileged, or protected-table-owner role.
func ApplyTenantRLS(db *gorm.DB, runtimeRole string) error {
	if db == nil {
		return errors.New("database connection is required")
	}
	if err := validateIdentifier(runtimeRole); err != nil {
		return fmt.Errorf("invalid runtime role: %w", err)
	}
	var sessionReplicationRole string
	if err := db.Raw(
		"SELECT pg_catalog.current_setting('session_replication_role')",
	).Scan(&sessionReplicationRole).Error; err != nil {
		return fmt.Errorf("inspect migration session_replication_role: %w", err)
	}
	if sessionReplicationRole != "origin" {
		return fmt.Errorf("migration session_replication_role is %q; expected origin", sessionReplicationRole)
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
		OID         int64 `gorm:"column:role_oid"`
		Exists      bool  `gorm:"column:role_exists"`
		Superuser   bool  `gorm:"column:superuser"`
		CreateRole  bool  `gorm:"column:create_role"`
		BypassRLS   bool  `gorm:"column:bypass_rls"`
		Replication bool  `gorm:"column:replication"`
	}
	if err := db.Raw(`
		SELECT
			COUNT(*) = 1 AS role_exists,
			COALESCE(min(oid::bigint), 0) AS role_oid,
			COALESCE(bool_or(rolsuper), false) AS superuser,
			COALESCE(bool_or(rolcreaterole), false) AS create_role,
			COALESCE(bool_or(rolbypassrls), false) AS bypass_rls,
			COALESCE(bool_or(rolreplication), false) AS replication
		FROM pg_catalog.pg_roles
		WHERE rolname = ?
	`, runtimeRole).Scan(&roleState).Error; err != nil {
		return fmt.Errorf("inspect runtime role: %w", err)
	}
	if !roleState.Exists || roleState.OID == 0 {
		return fmt.Errorf("runtime role %q does not exist", runtimeRole)
	}
	if roleState.Superuser || roleState.CreateRole || roleState.BypassRLS || roleState.Replication {
		return fmt.Errorf(
			"runtime role %q must be NOSUPERUSER, NOCREATEROLE, NOBYPASSRLS, and NOREPLICATION",
			runtimeRole,
		)
	}
	canDisableTriggers, err := roleCanSetSessionReplicationRole(db, roleState.OID)
	if err != nil {
		return fmt.Errorf("inspect runtime session-replication authority: %w", err)
	}
	if canDisableTriggers {
		return fmt.Errorf("runtime role %q has unauthorized session_replication_role authority or configuration", runtimeRole)
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
	if err := verifyNoDangerousRuntimeDefaultTablePrivileges(
		db,
		migrationRoleOID,
		roleState.OID,
	); err != nil {
		return fmt.Errorf("inspect runtime default-table authority: %w", err)
	}
	if err := verifyNoDangerousProtectedRuntimeTablePrivileges(
		db,
		installedTenantTables,
		runtimeRole,
	); err != nil {
		return fmt.Errorf("inspect runtime protected-table authority: %w", err)
	}
	installedTenantTableSet := make(map[string]struct{}, len(installedTenantTables))
	for _, table := range installedTenantTables {
		installedTenantTableSet[table] = struct{}{}
	}

	runtime := quoteIdentifier(runtimeRole)
	migrator := quoteIdentifier(migrationRole)

	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := revokeRuntimePublicSchemaCreate(tx, runtimeRole, roleState.OID); err != nil {
			return err
		}
		// The runtime process needs ordinary DML rights, while RLS determines
		// which rows are visible. Future tables inherit the same grants.
		if err := tx.Exec(fmt.Sprintf(
			"GRANT USAGE ON SCHEMA public TO %s", runtime,
		)).Error; err != nil {
			return err
		}
		if err := tx.Exec(fmt.Sprintf(
			"REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM %s", runtime,
		)).Error; err != nil {
			return err
		}
		if err := tx.Exec(
			"REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM PUBLIC",
		).Error; err != nil {
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
			"ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public REVOKE ALL PRIVILEGES ON TABLES FROM %s",
			migrator, runtime,
		)).Error; err != nil {
			return err
		}
		if err := tx.Exec(fmt.Sprintf(
			"ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public REVOKE ALL PRIVILEGES ON TABLES FROM PUBLIC",
			migrator,
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

		tenantExpr := "organization_id = NULLIF(pg_catalog.current_setting('app.current_organization_id', true), '')::uuid"
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
		if err := installPlatformComplianceGuards(tx, migrationRole, runtimeRole); err != nil {
			return err
		}
		if err := verifyExactProtectedRuntimeTablePrivileges(
			tx,
			installedTenantTables,
			runtimeRole,
		); err != nil {
			return err
		}

		return installRoutingFunctions(tx, runtime)
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted}); err != nil {
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
	var sessionReplicationRole string
	if err := db.Raw(
		"SELECT pg_catalog.current_setting('session_replication_role')",
	).Scan(&sessionReplicationRole).Error; err != nil {
		return fmt.Errorf("inspect runtime session_replication_role: %w", err)
	}
	if sessionReplicationRole != "origin" {
		return fmt.Errorf("runtime session_replication_role is %q; expected origin", sessionReplicationRole)
	}

	var roleState struct {
		Superuser   bool
		CreateRole  bool
		BypassRLS   bool
		Replication bool
	}
	if err := db.Raw(`
		SELECT
			rolsuper AS superuser,
			rolcreaterole AS create_role,
			rolbypassrls AS bypass_rls,
			rolreplication AS replication
		FROM pg_catalog.pg_roles
		WHERE rolname = current_user
	`).Scan(&roleState).Error; err != nil {
		return fmt.Errorf("inspect current database role: %w", err)
	}
	if roleState.Superuser || roleState.CreateRole || roleState.BypassRLS || roleState.Replication {
		return fmt.Errorf(
			"runtime role %q must be NOSUPERUSER, NOCREATEROLE, NOBYPASSRLS, and NOREPLICATION",
			currentRole,
		)
	}
	canDisableTriggers, err := roleCanSetSessionReplicationRole(db, identity.RuntimeRoleOID)
	if err != nil {
		return fmt.Errorf("inspect runtime session-replication authority: %w", err)
	}
	if canDisableTriggers {
		return fmt.Errorf("runtime role %q has unauthorized session_replication_role authority or configuration", currentRole)
	}

	var staleTenant bool
	if err := db.Raw(
		"SELECT NULLIF(pg_catalog.current_setting(?, true), '') IS NOT NULL",
		tenantSetting,
	).Scan(&staleTenant).Error; err != nil {
		return fmt.Errorf("inspect tenant connection state: %w", err)
	}
	if staleTenant {
		return errors.New("database pool contains a session-level tenant context")
	}
	var runtimeCanCreatePublic bool
	if err := db.Raw(
		"SELECT pg_catalog.has_schema_privilege(current_user, 'public', 'CREATE')",
	).Scan(&runtimeCanCreatePublic).Error; err != nil {
		return fmt.Errorf("inspect runtime public-schema authority: %w", err)
	}
	if runtimeCanCreatePublic {
		return fmt.Errorf("runtime role %q must not have CREATE on schema public", currentRole)
	}
	if err := verifyNoSettablePublicSchemaCreator(db, currentRole, identity.RuntimeRoleOID); err != nil {
		return err
	}

	// Prove the complete relation and policy catalogue before any query against
	// an application table can evaluate a tampered RLS expression.
	existingTenantTables, err := existingProtectedTenantTables(db)
	if err != nil {
		return fmt.Errorf("inspect protected tenant tables: %w", err)
	}
	if err := requireExactExistingProtectedTenantTables(
		db,
		protectedTenantTableNames(),
	); err != nil {
		return fmt.Errorf("verify protected tenant relation inventory: %w", err)
	}
	if err := verifyRuntimeCanonicalTenantPolicyInventory(
		db,
		existingTenantTables,
		currentRole,
	); err != nil {
		return fmt.Errorf("verify canonical tenant policy inventory: %w", err)
	}
	if err := verifyLegacyAdditiveSchemaContract(db); err != nil {
		return fmt.Errorf("verify identity-review schema contract: %w", err)
	}
	if err := verifyTenantPolicyFingerprint(db, existingTenantTables, currentRole); err != nil {
		return err
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
	if err := verifyPlatformComplianceGuards(db, currentRole); err != nil {
		return fmt.Errorf("verify platform compliance write barrier: %w", err)
	}

	if err := verifyLegacyAdditiveIndex(db, legacyAdditiveIndexContract{
		name:                 "uq_whatsapp_accounts_live_phone_id",
		table:                "whatsapp_accounts",
		columns:              "btrim(phone_id::text)",
		predicate:            "deleted_at IS NULL",
		dependencyColumns:    "deleted_at,phone_id",
		tableDependencyCount: 1,
		unique:               true,
	}); err != nil {
		return fmt.Errorf("WhatsApp Phone ID routing index is missing or invalid: %w", err)
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
			OR privileged_role.rolcreaterole
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

func tenantPolicyFingerprintProfiles(tables []string) (core, additive []string, err error) {
	additiveSet := make(map[string]struct{}, len(tenantRLSAdditiveProfileV1))
	for _, table := range tenantRLSAdditiveProfileV1 {
		if err := validateIdentifier(table); err != nil {
			return nil, nil, fmt.Errorf("invalid additive tenant table %q: %w", table, err)
		}
		if _, duplicate := additiveSet[table]; duplicate {
			return nil, nil, fmt.Errorf("duplicate additive tenant table %q", table)
		}
		additiveSet[table] = struct{}{}
	}
	seen := make(map[string]struct{}, len(tables))
	for _, table := range tables {
		if _, duplicate := seen[table]; duplicate {
			return nil, nil, fmt.Errorf("duplicate protected tenant table %q", table)
		}
		seen[table] = struct{}{}
		if _, optional := additiveSet[table]; optional {
			additive = append(additive, table)
		} else {
			core = append(core, table)
		}
	}
	sort.Strings(core)
	sort.Strings(additive)
	return core, additive, nil
}

func tenantPolicyAdditiveFingerprint(db *gorm.DB, tables []string) (string, error) {
	fingerprint, err := tenantPolicyFingerprint(db, tables)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("v%d:%s", tenantRLSAdditiveProfileVersion, fingerprint), nil
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
	coreTables, additiveTables, err := tenantPolicyFingerprintProfiles(tables)
	if err != nil {
		return fmt.Errorf("partition tenant policy fingerprint profiles: %w", err)
	}
	if len(additiveTables) != len(tenantRLSAdditiveProfileV1) {
		return fmt.Errorf(
			"additive tenant policy profile v%d is incomplete: got %d table(s), expected %d",
			tenantRLSAdditiveProfileVersion, len(additiveTables), len(tenantRLSAdditiveProfileV1),
		)
	}
	fingerprint, err := tenantPolicyFingerprint(tx, coreTables)
	if err != nil {
		return fmt.Errorf("compute version-5 tenant policy fingerprint: %w", err)
	}
	additiveFingerprint, err := tenantPolicyAdditiveFingerprint(tx, additiveTables)
	if err != nil {
		return fmt.Errorf("compute additive tenant policy fingerprint: %w", err)
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
		"DROP FUNCTION IF EXISTS public.rereply_tenant_policy_additive_fingerprint_v1()",
		fmt.Sprintf(`CREATE OR REPLACE FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1()
			RETURNS text
			LANGUAGE sql
			IMMUTABLE
			SET search_path = pg_catalog, public
			AS $function$
			  SELECT '%s'::text
			$function$`, additiveFingerprint),
		"REVOKE ALL ON FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1() FROM PUBLIC",
		fmt.Sprintf(
			"GRANT EXECUTE ON FUNCTION public.rereply_tenant_policy_additive_fingerprint_v1() TO %s",
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

func verifyTenantPolicyFingerprint(
	db *gorm.DB,
	tables []string,
	runtimeRole string,
) error {
	coreTables, additiveTables, err := tenantPolicyFingerprintProfiles(tables)
	if err != nil {
		return fmt.Errorf("partition tenant policy fingerprint profiles: %w", err)
	}
	if len(additiveTables) != len(tenantRLSAdditiveProfileV1) {
		return fmt.Errorf(
			"additive tenant policy profile v%d is incomplete: got %d table(s), expected %d",
			tenantRLSAdditiveProfileVersion, len(additiveTables), len(tenantRLSAdditiveProfileV1),
		)
	}
	actual, err := tenantPolicyFingerprint(db, coreTables)
	if err != nil {
		return fmt.Errorf("compute current tenant policy fingerprint: %w", err)
	}
	owner, err := verifyExactLegacyTenantFingerprintFunction(
		db,
		tenantPolicyFingerprintSignature,
		"rereply_tenant_policy_fingerprint",
		actual,
		runtimeRole,
		nil,
		false,
	)
	if err != nil {
		return fmt.Errorf("verify tenant policy fingerprint contract: %w", err)
	}
	if err := verifyProtectedTableFingerprintOwner(db, tables, owner.OID); err != nil {
		return err
	}
	runtimeRoleOID, err := exactDatabaseRoleOID(db, runtimeRole)
	if err != nil {
		return err
	}
	if err := verifyNoDangerousRuntimeDefaultTablePrivileges(db, owner.OID, runtimeRoleOID); err != nil {
		return err
	}
	return verifyTenantAdditivePolicyFingerprint(db, additiveTables, runtimeRole, owner)
}

func verifyProtectedTableFingerprintOwner(db *gorm.DB, tables []string, ownerOID int64) error {
	if len(tables) == 0 || ownerOID == 0 {
		return errors.New("protected-table fingerprint owner binding is missing")
	}
	placeholders := make([]string, len(tables))
	arguments := make([]any, 0, len(tables)+1)
	arguments = append(arguments, ownerOID)
	for index, table := range tables {
		if err := validateIdentifier(table); err != nil {
			return err
		}
		placeholders[index] = "?"
		arguments = append(arguments, table)
	}
	var count int64
	query := fmt.Sprintf(`
		SELECT COUNT(*)
		FROM pg_catalog.pg_class AS relation
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		WHERE relation.relowner = CAST(? AS oid)
		  AND namespace.nspname = 'public'
		  AND relation.relkind = 'r'
		  AND relation.relpersistence = 'p'
		  AND relation.relname IN (%s)
	`, strings.Join(placeholders, ", "))
	if err := db.Raw(query, arguments...).Scan(&count).Error; err != nil {
		return fmt.Errorf("inspect protected-table fingerprint owner binding: %w", err)
	}
	if count != int64(len(tables)) {
		return errors.New("tenant fingerprint owner does not own every protected table")
	}
	return nil
}

func verifyTenantAdditivePolicyFingerprint(
	db *gorm.DB,
	tables []string,
	runtimeRole string,
	coreOwner databaseRoleReference,
) error {
	expected, err := tenantPolicyAdditiveFingerprint(db, tables)
	if err != nil {
		return fmt.Errorf("compute additive tenant policy fingerprint: %w", err)
	}
	if _, err := verifyExactLegacyTenantFingerprintFunction(
		db,
		tenantAdditivePolicyFingerprintSignature,
		"rereply_tenant_policy_additive_fingerprint_v1",
		expected,
		runtimeRole,
		&coreOwner,
		false,
	); err != nil {
		return fmt.Errorf(
			"additive tenant policy profile v%d fingerprint contract is invalid: %w",
			tenantRLSAdditiveProfileVersion,
			err,
		)
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
	// GORM implements Transaction on an existing *sql.Tx as a savepoint and
	// ignores new isolation options. Reject nesting outright so a caller cannot
	// smuggle a REPEATABLE READ snapshot across the purpose-organization
	// teardown interlock.
	type transactionCommitter interface {
		Commit() error
		Rollback() error
	}
	if db.Statement != nil {
		if _, nested := db.Statement.ConnPool.(transactionCommitter); nested {
			return ErrTenantRLSRemovalTransaction
		}
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL search_path = pg_catalog, public").Error; err != nil {
			return fmt.Errorf("pin tenant RLS removal search path: %w", err)
		}
		var isolation string
		if err := tx.Raw("SHOW transaction_isolation").Scan(&isolation).Error; err != nil {
			return fmt.Errorf("inspect tenant RLS removal isolation: %w", err)
		}
		if strings.ToLower(strings.TrimSpace(isolation)) != "read committed" {
			return ErrTenantRLSRemovalTransaction
		}
		if err := removePlatformComplianceGuards(tx); err != nil {
			return err
		}
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
			"public.rereply_tenant_policy_additive_fingerprint_v1()",
			"public.rereply_rls_routing_version()",
		} {
			if err := tx.Exec("DROP FUNCTION IF EXISTS " + signature).Error; err != nil {
				return err
			}
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
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
