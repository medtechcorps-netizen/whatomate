// Command bootstrap builds an empty staging (or CI) database to the schema,
// seeds and tenant-RLS profile that production's PRE_DEPLOY
// `rereply rls-migrate` accepts on its verify-only path. Staging therefore
// never needs a production dump, a historical binary or a production
// connection: nothing it holds comes from production.
//
// It runs as the database's migration owner. On DigitalOcean that is the
// cluster's built-in doadmin, which owns the database the owner created in the
// console; no role is ever created with SQL. Before any write it refuses
// unless:
//
//   - app.environment is staging or test;
//   - database.migration_url and database.url are both set, connect to the
//     same database as two different roles with current_user = session_user,
//     and database.url connects as database.runtime_role;
//   - the server runs a PostgreSQL major the database package authorizes
//     (14 or 17);
//   - the owner is not a superuser, and the runtime role is NOSUPERUSER,
//     NOCREATEROLE, NOBYPASSRLS and NOREPLICATION and is not a member of a
//     role with any of those attributes;
//   - no role other than the owner is a direct or transitive member of the
//     owner or of the runtime role (the membership preflight). The platform
//     compliance verifier counts every such member, so it would otherwise
//     fail ApplyTenantRLS after the schema has been written. PostgreSQL 16+
//     makes a CREATEROLE non-superuser an ADMIN member of every role it
//     creates, so an owner role that doadmin created is refused here; the
//     owner's own membership in the runtime role (doadmin created it) is
//     allowed, as the verifier allows it;
//   - the owner's sessions start with session_replication_role = origin, the
//     runtime role can neither set session_replication_role nor has it
//     configured, and the owner's default privileges give the runtime role
//     no TRUNCATE, REFERENCES, TRIGGER or MAINTAIN on new tables;
//   - the owner owns the database, so no third role holds it or (on
//     PostgreSQL 15+, through pg_database_owner) its public schema, and the
//     owner can create in that schema;
//   - the default administrator is not admin@admin.com (which the migration
//     makes a super admin) and its password is neither config.example.toml's
//     nor shorter than 16 characters;
//   - the public schema holds no relation, function or type, so a production
//     database, or one this tool already built, is refused by construction.
//
// The version, role, session_replication_role and default-privilege checks
// are ApplyTenantRLS's own gates (internal/database/tenant.go), which would
// otherwise fire only after the schema has been written.
//
// Then, as the owner, it creates the legacy chatbot_flow_steps table that
// production still carries, runs database.RunMigrationWithProgress with the
// synthetic administrator, runs the same backfills as runRLSMigration in
// cmd/whatomate/main.go (main_test.go keeps the two lists equal), applies
// database.ApplyTenantRLS, and verifies the result through database.url.
//
// Its own output is fixed messages and counts, never a URL, a password or the
// administrator's address. It lives under release/, outside the schema guard's
// scanned roots (release/deployment/schema_change.py) and outside every release
// binary (placement_test.go).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/zerodha/logf"
	"gorm.io/gorm"
)

const (
	// exitComplete means the database was built and verified.
	exitComplete = 0
	// exitFailed means a step failed after the first write: drop and
	// recreate the database before trying again.
	exitFailed = 1
	// exitRefused means nothing was written.
	exitRefused = 2

	// exampleAdminPassword is config.example.toml's [default_admin] password.
	exampleAdminPassword = "replace-with-a-strong-random-password"
	// forbiddenAdminEmail is made a super admin by the migration itself.
	forbiddenAdminEmail   = "admin@admin.com"
	minAdminPasswordRunes = 16
	// minOwnerConnections leaves the migration a connection beside the one
	// that holds the bootstrap lock.
	minOwnerConnections = 4
	connectTimeout      = 30 * time.Second
	lockName            = "rereply staging bootstrap"
	logPrefix           = "staging-bootstrap: "
)

var (
	allowedEnvironments = map[string]bool{"staging": true, "test": true}
	// releaseMajors are the PostgreSQL majors supportedPostgresMajor in
	// internal/database/postgres.go authorizes; main_test.go keeps them equal.
	releaseMajors = map[int]bool{14: true, 17: true}
	// Same rule as the database package's identifier check.
	roleIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)
)

// refusal is an error raised before any write.
type refusal struct{ reason string }

func (r refusal) Error() string { return r.reason }

func refuse(format string, args ...any) error {
	return refusal{reason: fmt.Sprintf(format, args...)}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("staging-bootstrap", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "config.toml", "path to the private staging or CI config")
	if err := flags.Parse(args); err != nil {
		return exitRefused
	}
	if flags.NArg() != 0 {
		return report(stderr, refuse("unexpected arguments; the only flag is -config"))
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return report(stderr, refuse("cannot load the config: %v", err))
	}
	if err := checkConfig(cfg); err != nil {
		return report(stderr, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	owner, err := connect(ctx, cfg.Database, cfg.Database.MigrationURL, minOwnerConnections)
	if err != nil {
		return report(stderr, refuse("cannot connect with database.migration_url%s", sqlState(err)))
	}
	defer closeDB(owner)
	runtime, err := connect(ctx, cfg.Database, cfg.Database.URL, 0)
	if err != nil {
		return report(stderr, refuse("cannot connect with database.url%s", sqlState(err)))
	}
	defer closeDB(runtime)

	// Held for the whole run, so a concurrent bootstrap of this database
	// is refused instead of both passing the empty-schema check.
	unlock, err := lock(ctx, owner)
	if err != nil {
		return report(stderr, err)
	}
	defer unlock()

	if err := preflight(owner, runtime, cfg.Database.RuntimeRole); err != nil {
		return report(stderr, err)
	}

	logger := logf.New(logf.Opts{
		Writer:        stderr,
		Level:         logf.InfoLevel,
		DefaultFields: []any{"app", "staging-bootstrap"},
	})
	if err := bootstrap(owner, cfg, logger); err != nil {
		return report(stderr, err)
	}
	if err := database.VerifyTenantRLS(runtime, cfg.Database.RuntimeRole); err != nil {
		return report(stderr, fmt.Errorf("verify tenant RLS through database.url: %w", err))
	}
	counts, err := readCounts(owner)
	if err != nil {
		return report(stderr, fmt.Errorf("count the result: %w", err))
	}
	_, _ = fmt.Fprintf(stdout, "%sbootstrap complete %s\n", logPrefix, counts)
	return exitComplete
}

func report(stderr io.Writer, err error) int {
	var refused refusal
	if errors.As(err, &refused) {
		_, _ = fmt.Fprintf(stderr, "%srefusing: %s\n", logPrefix, refused.reason)
		return exitRefused
	}
	_, _ = fmt.Fprintf(stderr, "%sfailed after writing: %v\n", logPrefix, err)
	_, _ = fmt.Fprintf(stderr, "%sdrop and recreate the database before trying again\n", logPrefix)
	return exitFailed
}

// checkConfig holds every refusal that needs no database.
func checkConfig(cfg *config.Config) error {
	if !allowedEnvironments[cfg.App.Environment] {
		return refuse("app.environment must be staging or test")
	}
	migrationURL := strings.TrimSpace(cfg.Database.MigrationURL)
	runtimeURL := strings.TrimSpace(cfg.Database.URL)
	if migrationURL == "" || runtimeURL == "" {
		return refuse("database.migration_url and database.url are both required")
	}
	if migrationURL == runtimeURL {
		return refuse("database.migration_url and database.url are the same")
	}
	if !roleIdentifier.MatchString(cfg.Database.RuntimeRole) {
		return refuse("database.runtime_role is not a plain PostgreSQL identifier")
	}
	if strings.EqualFold(strings.TrimSpace(cfg.DefaultAdmin.Email), forbiddenAdminEmail) {
		return refuse("default_admin.email must not be %s", forbiddenAdminEmail)
	}
	if cfg.DefaultAdmin.Password == exampleAdminPassword {
		return refuse("default_admin.password is config.example.toml's")
	}
	if utf8.RuneCountInString(cfg.DefaultAdmin.Password) < minAdminPasswordRunes {
		return refuse("default_admin.password is shorter than %d characters", minAdminPasswordRunes)
	}
	return nil
}

func connect(ctx context.Context, base config.DatabaseConfig, url string, minConnections int) (*gorm.DB, error) {
	connection := base
	connection.URL = url
	if connection.MaxOpenConns < minConnections {
		connection.MaxOpenConns = minConnections
	}
	return database.NewPostgresWithContext(ctx, &connection, false)
}

func closeDB(db *gorm.DB) {
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// sqlState names a PostgreSQL error code, never the connection error's text,
// which can carry the host and role.
func sqlState(err error) string {
	var coded interface{ SQLState() string }
	if errors.As(err, &coded) && coded.SQLState() != "" {
		return " (SQLSTATE " + coded.SQLState() + ")"
	}
	return ""
}

func lock(ctx context.Context, owner *gorm.DB) (func(), error) {
	sqlDB, err := owner.DB()
	if err != nil {
		return nil, refuse("cannot open the migration owner's pool")
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, refuse("cannot hold a migration owner connection%s", sqlState(err))
	}
	var acquired bool
	if err := conn.QueryRowContext(ctx,
		"SELECT pg_catalog.pg_try_advisory_lock(pg_catalog.hashtextextended($1, 0))", lockName,
	).Scan(&acquired); err != nil {
		_ = conn.Close()
		return nil, refuse("cannot take the bootstrap lock%s", sqlState(err))
	}
	if !acquired {
		_ = conn.Close()
		return nil, refuse("another staging bootstrap is running against this database")
	}
	return func() {
		_, _ = conn.ExecContext(context.Background(),
			"SELECT pg_catalog.pg_advisory_unlock(pg_catalog.hashtextextended($1, 0))", lockName)
		_ = conn.Close()
	}, nil
}

type identity struct {
	CurrentRole string `gorm:"column:current_role_name"`
	SessionRole string `gorm:"column:session_role_name"`
	Database    string `gorm:"column:database_name"`
}

func readIdentity(db *gorm.DB) (identity, error) {
	var id identity
	err := db.Raw(`
		SELECT current_user::text AS current_role_name,
		       session_user::text AS session_role_name,
		       pg_catalog.current_database()::text AS database_name
	`).Scan(&id).Error
	return id, err
}

type roleState struct {
	RuntimeExists          bool  `gorm:"column:runtime_exists"`
	OwnerSuperuser         bool  `gorm:"column:owner_superuser"`
	RuntimeSuperuser       bool  `gorm:"column:runtime_superuser"`
	RuntimeCreateRole      bool  `gorm:"column:runtime_createrole"`
	RuntimeBypassRLS       bool  `gorm:"column:runtime_bypassrls"`
	RuntimeReplication     bool  `gorm:"column:runtime_replication"`
	RuntimePrivilegedRoles int64 `gorm:"column:runtime_privileged_roles"`
}

// membership counts the roles other than the owner that are direct or
// transitive members of the owner and of the runtime role. The recursion is
// the platform compliance verifier's (internal/database/platform_compliance.go,
// its role_membership CTE and unexpected_inheritance check).
type membership struct {
	OwnerMembers   int64 `gorm:"column:owner_members"`
	RuntimeMembers int64 `gorm:"column:runtime_members"`
}

type publicSchema struct {
	Schemas   int64 `gorm:"column:schemas"`
	Relations int64 `gorm:"column:relations"`
	Functions int64 `gorm:"column:functions"`
	Types     int64 `gorm:"column:types"`
}

// preflight holds every refusal that reads the database. It never writes.
func preflight(owner, runtime *gorm.DB, runtimeRole string) error {
	ownerID, err := readIdentity(owner)
	if err != nil {
		return refuse("cannot read the migration owner's identity%s", sqlState(err))
	}
	runtimeID, err := readIdentity(runtime)
	if err != nil {
		return refuse("cannot read the runtime identity%s", sqlState(err))
	}
	if ownerID.CurrentRole != ownerID.SessionRole {
		return refuse("database.migration_url's current_user differs from its session_user")
	}
	if runtimeID.CurrentRole != runtimeID.SessionRole {
		return refuse("database.url's current_user differs from its session_user")
	}
	if ownerID.SessionRole == runtimeID.SessionRole {
		return refuse("database.migration_url and database.url connect as the same role")
	}
	if runtimeID.SessionRole != runtimeRole {
		return refuse("database.url does not connect as database.runtime_role")
	}
	if ownerID.Database != runtimeID.Database {
		return refuse("database.migration_url and database.url name different databases")
	}

	var major int
	if err := owner.Raw(
		"SELECT pg_catalog.current_setting('server_version_num')::integer / 10000",
	).Scan(&major).Error; err != nil {
		return refuse("cannot read the PostgreSQL version%s", sqlState(err))
	}
	if !releaseMajors[major] {
		return refuse("PostgreSQL %d is not a release-authorized major (14 or 17)", major)
	}

	var roles roleState
	if err := owner.Raw(`
		SELECT
			runtime_role.oid IS NOT NULL AS runtime_exists,
			owner_role.rolsuper AS owner_superuser,
			COALESCE(runtime_role.rolsuper, false) AS runtime_superuser,
			COALESCE(runtime_role.rolcreaterole, false) AS runtime_createrole,
			COALESCE(runtime_role.rolbypassrls, false) AS runtime_bypassrls,
			COALESCE(runtime_role.rolreplication, false) AS runtime_replication,
			(
				SELECT COUNT(*)
				FROM pg_catalog.pg_roles AS privileged
				WHERE runtime_role.oid IS NOT NULL
				  AND privileged.oid <> runtime_role.oid
				  AND (privileged.rolsuper OR privileged.rolcreaterole
				       OR privileged.rolbypassrls OR privileged.rolreplication)
				  AND pg_catalog.pg_has_role(runtime_role.oid, privileged.oid, 'MEMBER')
			) AS runtime_privileged_roles
		FROM pg_catalog.pg_roles AS owner_role
		LEFT JOIN pg_catalog.pg_roles AS runtime_role ON runtime_role.rolname = ?
		WHERE owner_role.rolname = current_user
	`, runtimeRole).Scan(&roles).Error; err != nil {
		return refuse("cannot read the role attributes%s", sqlState(err))
	}
	switch {
	case !roles.RuntimeExists:
		return refuse("database.runtime_role does not exist in the migration owner's cluster")
	case roles.OwnerSuperuser:
		return refuse("the migration owner is a superuser")
	case roles.RuntimeSuperuser || roles.RuntimeCreateRole || roles.RuntimeBypassRLS || roles.RuntimeReplication:
		return refuse("the runtime role must be NOSUPERUSER NOCREATEROLE NOBYPASSRLS NOREPLICATION")
	case roles.RuntimePrivilegedRoles != 0:
		return refuse("the runtime role is a member of privileged roles=%d", roles.RuntimePrivilegedRoles)
	}

	var members membership
	if err := owner.Raw(`
		WITH RECURSIVE role_membership(member, inherited_role) AS (
			SELECT membership.member, membership.roleid
			FROM pg_catalog.pg_auth_members AS membership
			UNION
			SELECT role_membership.member, membership.roleid
			FROM role_membership
			JOIN pg_catalog.pg_auth_members AS membership
			  ON membership.member = role_membership.inherited_role
		), owner_role AS (
			SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user
		), runtime_role AS (
			SELECT oid FROM pg_catalog.pg_roles WHERE rolname = ?
		)
		SELECT
			(
				SELECT COUNT(DISTINCT role_membership.member)
				FROM role_membership, owner_role
				WHERE role_membership.inherited_role = owner_role.oid
				  AND role_membership.member <> owner_role.oid
			) AS owner_members,
			(
				SELECT COUNT(DISTINCT role_membership.member)
				FROM role_membership, owner_role, runtime_role
				WHERE role_membership.inherited_role = runtime_role.oid
				  AND role_membership.member <> owner_role.oid
			) AS runtime_members
	`, runtimeRole).Scan(&members).Error; err != nil {
		return refuse("cannot read the role memberships%s", sqlState(err))
	}
	if members.OwnerMembers != 0 || members.RuntimeMembers != 0 {
		return refuse("membership preflight: roles other than the migration owner are members of the owner=%d runtime=%d",
			members.OwnerMembers, members.RuntimeMembers)
	}

	if err := checkAuthority(owner, runtimeRole, major); err != nil {
		return err
	}

	public, err := readPublicSchema(owner)
	if err != nil {
		return refuse("cannot inspect the public schema%s", sqlState(err))
	}
	if public.Schemas != 1 {
		return refuse("the public schema is missing")
	}
	var creatable bool
	if err := owner.Raw(`
		SELECT COALESCE(pg_catalog.bool_or(pg_catalog.has_schema_privilege(namespace.oid, 'CREATE')), false)
		FROM pg_catalog.pg_namespace AS namespace
		WHERE namespace.nspname = 'public'
	`).Scan(&creatable).Error; err != nil {
		return refuse("cannot read the public schema's privileges%s", sqlState(err))
	}
	if !creatable {
		return refuse("the migration owner cannot create in the public schema")
	}
	if public.Relations != 0 || public.Functions != 0 || public.Types != 0 {
		return refuse("public schema is not empty relations=%d functions=%d types=%d",
			public.Relations, public.Functions, public.Types)
	}
	return nil
}

type authority struct {
	OwnsDatabase         bool   `gorm:"column:owns_database"`
	OwnerReplicationRole string `gorm:"column:owner_replication_role"`
	RuntimeReplication   bool   `gorm:"column:runtime_replication_configured"`
	RuntimeDefaultGrants int64  `gorm:"column:runtime_default_grants"`
}

// checkAuthority mirrors ApplyTenantRLS's session_replication_role and
// default-privilege gates (internal/database/tenant.go, which calls
// roleCanSetSessionReplicationRole and
// verifyNoDangerousRuntimeDefaultTablePrivileges in postgres.go), and
// requires the owner to own the database: ApplyTenantRLS does not check that,
// but a third role that owns the database could create in its public schema.
// It never writes.
func checkAuthority(owner *gorm.DB, runtimeRole string, major int) error {
	var state authority
	if err := owner.Raw(`
		WITH owner_role AS (
			SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user
		), runtime_role AS (
			SELECT oid FROM pg_catalog.pg_roles WHERE rolname = ?
		)
		SELECT
			COALESCE((
				SELECT database.datdba = owner_role.oid
				FROM pg_catalog.pg_database AS database, owner_role
				WHERE database.datname = pg_catalog.current_database()
			), false) AS owns_database,
			pg_catalog.current_setting('session_replication_role') AS owner_replication_role,
			EXISTS (
				SELECT 1
				FROM pg_catalog.pg_db_role_setting AS role_setting
				CROSS JOIN LATERAL pg_catalog.pg_options_to_table(role_setting.setconfig)
				  AS option(option_name, option_value)
				WHERE role_setting.setrole IN (0::oid, (SELECT oid FROM runtime_role))
				  AND role_setting.setdatabase IN (
					0::oid,
					(SELECT database.oid FROM pg_catalog.pg_database AS database
					 WHERE database.datname = pg_catalog.current_database())
				  )
				  AND option.option_name = 'session_replication_role'
				  AND option.option_value <> 'origin'
			) AS runtime_replication_configured,
			(
				SELECT COUNT(*)
				FROM pg_catalog.pg_default_acl AS defaults
				CROSS JOIN LATERAL pg_catalog.aclexplode(defaults.defaclacl) AS grant_state
				CROSS JOIN owner_role
				CROSS JOIN runtime_role
				WHERE defaults.defaclrole = owner_role.oid
				  AND defaults.defaclobjtype = 'r'
				  AND (
					defaults.defaclnamespace = 0
					OR defaults.defaclnamespace = (
						SELECT oid FROM pg_catalog.pg_namespace WHERE nspname = 'public'
					)
				  )
				  AND grant_state.privilege_type IN ('TRUNCATE', 'REFERENCES', 'TRIGGER', 'MAINTAIN')
				  AND (
					grant_state.grantee = 0
					OR grant_state.grantee = runtime_role.oid
					OR pg_catalog.pg_has_role(runtime_role.oid, grant_state.grantee, 'MEMBER')
				  )
			) AS runtime_default_grants
	`, runtimeRole).Scan(&state).Error; err != nil {
		return refuse("cannot read the migration owner's authority%s", sqlState(err))
	}
	if state.OwnerReplicationRole != "origin" {
		return refuse("the migration owner's session_replication_role is not origin")
	}
	settable := false
	// PostgreSQL 14 has no parameter privileges.
	if major != 14 {
		if err := owner.Raw(`
			WITH RECURSIVE settable_role(role_oid, path, can_set) AS (
				SELECT role.oid, ARRAY[role.oid]::oid[], true
				FROM pg_catalog.pg_roles AS role
				WHERE role.rolname = ?
				UNION ALL
				SELECT membership.roleid,
					settable_role.path || membership.roleid,
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
				  AND (
					pg_catalog.has_parameter_privilege(
						CAST(role_oid AS oid), 'session_replication_role', 'SET'
					)
					OR pg_catalog.has_parameter_privilege(
						CAST(role_oid AS oid), 'session_replication_role', 'ALTER SYSTEM'
					)
				  )
			)
		`, runtimeRole).Scan(&settable).Error; err != nil {
			return refuse("cannot read the runtime role's parameter privileges%s", sqlState(err))
		}
	}
	if state.RuntimeReplication || settable {
		return refuse("the runtime role can set or has configured session_replication_role")
	}
	if state.RuntimeDefaultGrants != 0 {
		return refuse("the migration owner's default privileges give the runtime role dangerous table grants=%d",
			state.RuntimeDefaultGrants)
	}
	if !state.OwnsDatabase {
		return refuse("the migration owner does not own the database")
	}
	return nil
}

func readPublicSchema(db *gorm.DB) (publicSchema, error) {
	var public publicSchema
	err := db.Raw(`
		SELECT
			(SELECT COUNT(*) FROM pg_catalog.pg_namespace WHERE nspname = 'public') AS schemas,
			(SELECT COUNT(*) FROM pg_catalog.pg_class AS relation
			 JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			 WHERE namespace.nspname = 'public') AS relations,
			(SELECT COUNT(*) FROM pg_catalog.pg_proc AS routine
			 JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = routine.pronamespace
			 WHERE namespace.nspname = 'public') AS functions,
			(SELECT COUNT(*) FROM pg_catalog.pg_type AS datatype
			 JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = datatype.typnamespace
			 WHERE namespace.nspname = 'public') AS types
	`).Scan(&public).Error
	return public, err
}

// bootstrap writes. Every step runs as the migration owner.
func bootstrap(owner *gorm.DB, cfg *config.Config, logger logf.Logger) error {
	// 1. Production still carries the pre-graph legacy table, and the strict
	// verifier's protected-relation inventory requires it.
	if err := inMigrationSession(owner, func(session *gorm.DB) error {
		return session.AutoMigrate(&models.ChatbotFlowStep{})
	}); err != nil {
		return fmt.Errorf("legacy chatbot_flow_steps: %w", err)
	}
	// 2. Schema, indexes and seeds, with the synthetic administrator.
	if err := database.RunMigrationWithProgress(owner, &cfg.DefaultAdmin); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	// 3 and 4. runRLSMigration's backfills, then the tenant policies, on one
	// connection, as the RLS migration coordinator runs them.
	return inMigrationSession(owner, func(session *gorm.DB) error {
		if err := backfill(session, logger); err != nil {
			return err
		}
		if err := database.ApplyTenantRLS(session, cfg.Database.RuntimeRole); err != nil {
			return fmt.Errorf("apply tenant RLS: %w", err)
		}
		return nil
	})
}

// backfill runs, in order, the backfills runRLSMigration in
// cmd/whatomate/main.go passes to the RLS migration coordinator.
// TestBackfillsMatchRunRLSMigration keeps the two lists equal.
func backfill(session *gorm.DB, logger logf.Logger) error {
	if err := handlers.BackfillChatbotFlowGraph(session, logger); err != nil {
		return fmt.Errorf("chatbot flow graph backfill: %w", err)
	}
	if _, err := channel.BackfillLegacyWhatsAppInbox(session, 500); err != nil {
		return fmt.Errorf("legacy WhatsApp omnichannel backfill: %w", err)
	}
	return nil
}

// inMigrationSession pins one owner connection with the migration search path
// (public, pg_temp) that the database package's migration session uses.
func inMigrationSession(owner *gorm.DB, step func(*gorm.DB) error) error {
	return owner.Connection(func(connection *gorm.DB) (err error) {
		session := connection.Session(&gorm.Session{NewDB: true})
		var configured string
		if err := session.Raw(
			"SELECT pg_catalog.set_config('search_path', 'public, pg_temp', false)",
		).Scan(&configured).Error; err != nil {
			return fmt.Errorf("set the migration search path: %w", err)
		}
		defer func() {
			if resetErr := session.Exec("RESET search_path").Error; resetErr != nil {
				err = errors.Join(err, fmt.Errorf("reset the migration search path: %w", resetErr))
			}
		}()
		var schema string
		if err := session.Raw("SELECT pg_catalog.current_schema()::text").Scan(&schema).Error; err != nil {
			return fmt.Errorf("read the migration schema: %w", err)
		}
		if schema != "public" {
			return errors.New("the migration session does not resolve to the public schema")
		}
		return step(session)
	})
}

type counts struct {
	Tables        int64 `gorm:"column:tables"`
	Policies      int64 `gorm:"column:policies"`
	Permissions   int64 `gorm:"column:permissions"`
	SystemRoles   int64 `gorm:"column:system_roles"`
	Users         int64 `gorm:"column:users"`
	Organizations int64 `gorm:"column:organizations"`
	Resellers     int64 `gorm:"column:resellers"`
}

func (c counts) String() string {
	return fmt.Sprintf("tables=%d policies=%d permissions=%d system_roles=%d users=%d organizations=%d resellers=%d",
		c.Tables, c.Policies, c.Permissions, c.SystemRoles, c.Users, c.Organizations, c.Resellers)
}

func readCounts(owner *gorm.DB) (counts, error) {
	var result counts
	err := owner.Raw(`
		SELECT
			(SELECT COUNT(*) FROM pg_catalog.pg_class AS relation
			 JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			 WHERE namespace.nspname = 'public' AND relation.relkind IN ('r', 'p')) AS tables,
			(SELECT COUNT(*) FROM pg_catalog.pg_policy AS policy
			 JOIN pg_catalog.pg_class AS relation ON relation.oid = policy.polrelid
			 JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			 WHERE namespace.nspname = 'public') AS policies,
			(SELECT COUNT(*) FROM public.permissions) AS permissions,
			(SELECT COUNT(*) FROM public.custom_roles WHERE is_system) AS system_roles,
			(SELECT COUNT(*) FROM public.users) AS users,
			(SELECT COUNT(*) FROM public.organizations) AS organizations,
			(SELECT COUNT(*) FROM public.resellers) AS resellers
	`).Scan(&result).Error
	return result, err
}
