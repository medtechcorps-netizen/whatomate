package database_test

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/require"
	"github.com/zerodha/logf"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// All writable bootstrap machinery is test-only. Capture never imports this
// package. A test process builds one empty template and clones it per test; it
// never bootstraps, truncates, or adopts the database named by TEST_DATABASE_URL.
var catalogTemplate struct {
	once                    sync.Once
	err                     error
	admin                   *gorm.DB
	base                    *url.URL
	name, owner, runtime    string
	ownerPassword, password string
	created                 bool
	ownerCreated            bool
	runtimeCreated          bool
}

func TestMain(m *testing.M) {
	code := m.Run()
	if err := cleanupCatalogTemplate(); err != nil {
		fmt.Fprintln(os.Stderr, "catalog test template cleanup failed")
		code = 1
	}
	os.Exit(code)
}

func catalogTestURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.User == nil || u.Path == "" {
		return nil, errors.New("catalog tests require a PostgreSQL test URL")
	}
	// CI and the explicitly created disposable local cluster are loopback-only.
	// No catalog integration test may use a remote/provider endpoint.
	if host := u.Hostname(); host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return nil, errors.New("catalog tests require a loopback synthetic database")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errors.New("catalog tests require valid local connection options")
	}
	// pgx accepts query-string host/user/database overrides. Do not permit a
	// loopback URL to redirect these writable test-only operations elsewhere.
	for key, values := range q {
		switch key {
		case "sslmode", "connect_timeout", "statement_cache_capacity", "default_query_exec_mode", "default_transaction_read_only":
		default:
			return nil, errors.New("catalog tests refuse connection overrides")
		}
		if len(values) != 1 {
			return nil, errors.New("catalog tests refuse duplicate connection options")
		}
	}
	q.Set("statement_cache_capacity", "0")
	q.Set("default_query_exec_mode", "describe_exec")
	q.Set("connect_timeout", "10")
	u.RawQuery = q.Encode()
	return u, nil
}

func catalogConnect(u *url.URL) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(u.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, errors.New("connect to synthetic catalog database")
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(5)
	sqlDB.SetMaxIdleConns(2)
	return db, nil
}

func catalogClose(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	s, err := db.DB()
	if err != nil {
		return err
	}
	return s.Close()
}

func catalogRoleURL(base *url.URL, name, role, password string) *url.URL {
	u := *base
	u.Path, u.RawPath = "/"+name, ""
	u.User = url.UserPassword(role, password)
	return &u
}

func initCatalogTemplate(base *url.URL) error {
	var err error
	catalogTemplate.base = base
	catalogTemplate.admin, err = catalogConnect(base)
	if err != nil {
		return err
	}
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	catalogTemplate.name = "rereply_catalog_template_" + id[:16]
	catalogTemplate.owner = "catalog_owner_" + id[:16]
	catalogTemplate.runtime = "catalog_runtime_" + id[:16]
	catalogTemplate.ownerPassword = "synthetic_owner_" + id
	catalogTemplate.password = "synthetic_runtime_" + id
	a := catalogTemplate.admin
	if err := a.Exec("CREATE ROLE " + catalogTemplate.owner + " LOGIN PASSWORD '" + catalogTemplate.ownerPassword + "' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION").Error; err != nil {
		return err
	}
	catalogTemplate.ownerCreated = true
	if err := a.Exec("CREATE ROLE " + catalogTemplate.runtime + " LOGIN PASSWORD '" + catalogTemplate.password + "' NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOBYPASSRLS NOREPLICATION").Error; err != nil {
		return err
	}
	catalogTemplate.runtimeCreated = true
	if err := a.Exec("CREATE DATABASE " + catalogTemplate.name + " OWNER " + catalogTemplate.owner).Error; err != nil {
		return err
	}
	catalogTemplate.created = true
	u := *base
	u.Path, u.RawPath = "/"+catalogTemplate.name, ""
	admin, err := catalogConnect(&u)
	if err != nil {
		return err
	}
	if err := admin.Exec("ALTER SCHEMA public OWNER TO " + catalogTemplate.owner).Error; err != nil {
		_ = catalogClose(admin)
		return err
	}
	if err := catalogClose(admin); err != nil {
		return err
	}
	owner, err := catalogConnect(catalogRoleURL(base, catalogTemplate.name, catalogTemplate.owner, catalogTemplate.ownerPassword))
	if err != nil {
		return err
	}
	defer catalogClose(owner)
	runtime, err := catalogConnect(catalogRoleURL(base, catalogTemplate.name, catalogTemplate.runtime, catalogTemplate.password))
	if err != nil {
		return err
	}
	defer catalogClose(runtime)
	return BootstrapEmptyForTest(owner, runtime, catalogTemplate.runtime)
}

func cleanupCatalogTemplate() error {
	if catalogTemplate.admin == nil {
		return nil
	}
	a := catalogTemplate.admin
	var err error
	if catalogTemplate.created {
		err = errors.Join(err, a.Exec("DROP DATABASE "+catalogTemplate.name+" WITH (FORCE)").Error)
	}
	if catalogTemplate.runtimeCreated {
		err = errors.Join(err, a.Exec("DROP ROLE "+catalogTemplate.runtime).Error)
	}
	if catalogTemplate.ownerCreated {
		err = errors.Join(err, a.Exec("DROP ROLE "+catalogTemplate.owner).Error)
	}
	return errors.Join(err, catalogClose(a))
}

type catalogFixture struct {
	owner, runtime *gorm.DB
	runtimeDSN     string
	name           string
}

func ensureCatalogTemplate(t *testing.T) {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	base, err := catalogTestURL(raw)
	require.NoError(t, err)
	catalogTemplate.once.Do(func() { catalogTemplate.err = initCatalogTemplate(base) })
	require.NoError(t, catalogTemplate.err)
	require.Equal(t, catalogTemplate.base.String(), base.String(), "template endpoint must not change within a test process")

}

func catalogClone(t *testing.T) catalogFixture {
	t.Helper()
	ensureCatalogTemplate(t)
	base := catalogTemplate.base
	var err error
	name := "rereply_catalog_clone_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	require.NoError(t, catalogTemplate.admin.Exec("CREATE DATABASE "+name+" TEMPLATE "+catalogTemplate.name+" OWNER "+catalogTemplate.owner).Error)
	f := catalogFixture{name: name}
	t.Cleanup(func() {
		require.NoError(t, catalogClose(f.runtime))
		require.NoError(t, catalogClose(f.owner))
		require.NoError(t, catalogTemplate.admin.Exec("DROP DATABASE "+name+" WITH (FORCE)").Error)
	})
	f.owner, err = catalogConnect(catalogRoleURL(base, name, catalogTemplate.owner, catalogTemplate.ownerPassword))
	require.NoError(t, err)
	u := catalogRoleURL(base, name, catalogTemplate.runtime, catalogTemplate.password)
	f.runtimeDSN = u.String()
	f.runtime, err = catalogConnect(u)
	require.NoError(t, err)
	return f
}

func catalogEmptyTarget(t *testing.T) catalogFixture {
	t.Helper()
	ensureCatalogTemplate(t)
	name := "rereply_catalog_empty_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	require.NoError(t, catalogTemplate.admin.Exec("CREATE DATABASE "+name+" TEMPLATE template0 OWNER "+catalogTemplate.owner).Error)
	f := catalogFixture{name: name}
	t.Cleanup(func() {
		require.NoError(t, catalogClose(f.runtime))
		require.NoError(t, catalogClose(f.owner))
		require.NoError(t, catalogTemplate.admin.Exec("DROP DATABASE "+name+" WITH (FORCE)").Error)
	})
	base := *catalogTemplate.base
	base.Path, base.RawPath = "/"+name, ""
	admin, err := catalogConnect(&base)
	require.NoError(t, err)
	require.NoError(t, admin.Exec("ALTER SCHEMA public OWNER TO "+catalogTemplate.owner).Error)
	require.NoError(t, catalogClose(admin))
	f.owner, err = catalogConnect(catalogRoleURL(catalogTemplate.base, name, catalogTemplate.owner, catalogTemplate.ownerPassword))
	require.NoError(t, err)
	runtimeURL := catalogRoleURL(catalogTemplate.base, name, catalogTemplate.runtime, catalogTemplate.password)
	f.runtimeDSN = runtimeURL.String()
	f.runtime, err = catalogConnect(runtimeURL)
	require.NoError(t, err)
	return f
}

// BootstrapEmptyForTest mirrors release/staging/bootstrap's proven synthetic
// sequence. The public schema must be empty before its first write. It is only
// compiled into this external test binary, never a release command or package.
func BootstrapEmptyForTest(owner, runtime *gorm.DB, runtimeRole string) error {
	var nonempty int64
	if err := owner.Raw(`SELECT
	 (SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public') +
	 (SELECT count(*) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public') +
	 (SELECT count(*) FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname='public') +
	 (SELECT count(*) FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_namespace n ON n.oid=d.refobjid WHERE d.refclassid='pg_catalog.pg_namespace'::regclass AND n.nspname='public')`).Scan(&nonempty).Error; err != nil {
		return err
	}
	if nonempty != 0 {
		return errors.New("catalog bootstrap requires an empty public schema")
	}
	var permitted bool
	if err := owner.Raw(`SELECT NOT r.rolsuper AND current_user=session_user
	 AND d.datdba=r.oid AND n.nspowner=r.oid
	 AND EXISTS (SELECT 1 FROM pg_catalog.pg_roles runtime WHERE runtime.rolname=?
	  AND runtime.oid<>r.oid AND NOT runtime.rolsuper AND NOT runtime.rolcreaterole
	  AND NOT runtime.rolbypassrls AND NOT runtime.rolreplication
	  AND NOT pg_catalog.pg_has_role(runtime.oid,r.oid,'MEMBER')
	  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles privileged
	   WHERE (privileged.rolsuper OR privileged.rolcreaterole OR privileged.rolbypassrls OR privileged.rolreplication)
	    AND pg_catalog.pg_has_role(runtime.oid,privileged.oid,'MEMBER')))
	 FROM pg_catalog.pg_roles r CROSS JOIN pg_catalog.pg_database d CROSS JOIN pg_catalog.pg_namespace n
	 WHERE r.rolname=current_user AND d.datname=current_database() AND n.nspname='public'`, runtimeRole).Scan(&permitted).Error; err != nil {
		return err
	}
	if !permitted {
		return errors.New("catalog bootstrap requires the isolated non-superuser database and schema owner")
	}
	if err := owner.AutoMigrate(&models.ChatbotFlowStep{}); err != nil {
		return err
	}
	admin := config.DefaultAdminConfig{Email: "catalog@example.invalid", Password: "synthetic_catalog_password_only", FullName: "Synthetic Catalog"}
	if err := database.RunMigrationWithProgress(owner, &admin); err != nil {
		return err
	}
	log := logf.New(logf.Opts{Writer: io.Discard})
	if err := owner.Connection(func(connection *gorm.DB) error {
		if err := handlers.BackfillChatbotFlowGraph(connection, log); err != nil {
			return err
		}
		if _, err := channel.BackfillLegacyWhatsAppInbox(connection, 500); err != nil {
			return err
		}
		return database.ApplyTenantRLS(connection, runtimeRole)
	}); err != nil {
		return err
	}
	if runtime != nil {
		return database.VerifyTenantRLS(runtime, runtimeRole)
	}
	// The explicit export caller supplies only the owner connection. Verify
	// the resulting future catalog through the real migration coordinator;
	// runtime login/startup is the compatibility consumer's separate proof.
	return database.RunRLSMigrationCoordinator(owner, &admin, runtimeRole,
		func(*gorm.DB) error { return errors.New("export unexpectedly selected a mutation callback") },
		func() error { return nil })
}

func TestTenantRLS_CatalogBootstrapEmptyReachesExactFutureProfile(t *testing.T) {
	f := catalogClone(t)
	require.NoError(t, database.VerifyTenantRLS(f.runtime, catalogTemplate.runtime))
	backfills, verifications := 0, 0
	// The production coordinator must choose its verify-only future-profile
	// branch, not merely tolerate the resulting table shapes at startup.
	require.NoError(t, database.RunRLSMigrationCoordinator(f.owner, &config.DefaultAdminConfig{}, catalogTemplate.runtime,
		func(*gorm.DB) error { backfills++; return errors.New("unexpected migration callback") },
		func() error { verifications++; return database.VerifyTenantRLS(f.runtime, catalogTemplate.runtime) }))
	require.Zero(t, backfills)
	require.Equal(t, 1, verifications)
}

func TestTenantRLS_CatalogBootstrapRefusesNonEmptyBeforeMutation(t *testing.T) {
	f := catalogClone(t)
	var before, after string
	const state = `SELECT md5(string_agg(c.oid::text || ':' || c.relname, ',' ORDER BY c.oid)) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'`
	require.NoError(t, f.owner.Raw(state).Scan(&before).Error)
	require.ErrorContains(t, BootstrapEmptyForTest(f.owner, f.runtime, catalogTemplate.runtime), "empty public schema")
	require.NoError(t, f.owner.Raw(state).Scan(&after).Error)
	require.Equal(t, before, after)
	require.NoError(t, database.VerifyTenantRLS(f.runtime, catalogTemplate.runtime))
}

func TestTenantRLS_CatalogBootstrapRefusesCollationBeforeMutation(t *testing.T) {
	f := catalogEmptyTarget(t)
	require.NoError(t, f.owner.Exec(`CREATE COLLATION public.synthetic_preexisting FROM pg_catalog."C"`).Error)
	const shape = `SELECT md5(COALESCE(string_agg(d.classid::text||':'||d.objid::text||':'||d.objsubid::text,',' ORDER BY d.classid,d.objid,d.objsubid),'')) FROM pg_depend d JOIN pg_namespace n ON n.oid=d.refobjid WHERE d.refclassid='pg_namespace'::regclass AND n.nspname='public'`
	var before, after string
	require.NoError(t, f.owner.Raw(shape).Scan(&before).Error)
	require.ErrorContains(t, BootstrapEmptyForTest(f.owner, f.runtime, catalogTemplate.runtime), "empty public schema")
	require.NoError(t, f.owner.Raw(shape).Scan(&after).Error)
	require.Equal(t, before, after)
	var ordinary int64
	require.NoError(t, f.owner.Raw(`SELECT (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public')+(SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public')+(SELECT count(*) FROM pg_type ty JOIN pg_namespace n ON n.oid=ty.typnamespace WHERE n.nspname='public')`).Scan(&ordinary).Error)
	require.Zero(t, ordinary)
}

// TestCompatBootstrapExport bootstraps the caller's explicitly named, empty,
// disposable loopback database, then leaves it intact for the compat consumer.
// COMPAT_BOOTSTRAP_DSN must log in as its non-superuser database/schema owner;
// COMPAT_BOOTSTRAP_RUNTIME_ROLE (default rereply_app) must already exist and be
// distinct and unprivileged. The caller owns database/role creation and cleanup.
// No URL, password or private descriptor is printed. This export verifies the
// future catalog; the consumer separately proves runtime login and old/new code.
func TestCompatBootstrapExport(t *testing.T) {
	raw := os.Getenv("COMPAT_BOOTSTRAP_DSN")
	if raw == "" {
		t.Skip("COMPAT_BOOTSTRAP_DSN not set")
	}
	base, err := catalogTestURL(raw)
	require.NoError(t, err)
	owner, err := catalogConnect(base)
	require.NoError(t, err)
	defer catalogClose(owner)
	runtimeRole := os.Getenv("COMPAT_BOOTSTRAP_RUNTIME_ROLE")
	if runtimeRole == "" {
		runtimeRole = "rereply_app"
	}
	// Deliberately do not print driver errors: the explicit export connection
	// is an input, even though it must point only at a local synthetic target.
	if err := BootstrapEmptyForTest(owner, nil, runtimeRole); err != nil {
		t.Fatal("synthetic compatibility bootstrap refused or failed")
	}
	t.Log("synthetic compatibility bootstrap exported and future catalog verified")
}

func TestCatalogSyntheticURLBoundary(t *testing.T) {
	for _, raw := range []string{
		"postgres://owner:synthetic@provider.invalid/test",
		"postgres://owner:synthetic@127.0.0.1/test?host=provider.invalid",
		"postgres://owner:synthetic@127.0.0.1/test?dbname=other",
		"postgres://owner:synthetic@127.0.0.1/test?sslmode=disable&sslmode=require",
		"postgres://owner:synthetic@127.0.0.1/test?options=-c%20role%3Dpostgres",
	} {
		_, err := catalogTestURL(raw)
		require.Error(t, err)
	}
	_, err := catalogTestURL("postgres://owner:synthetic@127.0.0.1:5432/test?sslmode=disable")
	require.NoError(t, err)
}
