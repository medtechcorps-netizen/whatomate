package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/knadh/koanf/parsers/toml"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const testAdminPassword = "synthetic-admin-password-0123456789"

// validConfig is a config every check in checkConfig accepts.
func validConfig() *config.Config {
	return &config.Config{
		App: config.AppConfig{Environment: "test"},
		Database: config.DatabaseConfig{
			URL:          "postgres://runtime@127.0.0.1:1/rereply",
			MigrationURL: "postgres://owner@127.0.0.1:1/rereply",
			RuntimeRole:  "rereply_app",
		},
		DefaultAdmin: config.DefaultAdminConfig{Email: "staging-admin@example.com", Password: testAdminPassword},
	}
}

func TestCheckConfigRefusals(t *testing.T) {
	require.NoError(t, checkConfig(validConfig()))
	staging := validConfig()
	staging.App.Environment = "staging"
	require.NoError(t, checkConfig(staging))
	exactly16 := validConfig()
	exactly16.DefaultAdmin.Password = strings.Repeat("é", minAdminPasswordRunes)
	require.NoError(t, checkConfig(exactly16))

	cases := map[string]struct {
		mutate func(*config.Config)
		reason string
	}{
		"production":            {func(c *config.Config) { c.App.Environment = "production" }, "app.environment must be staging or test"},
		"development":           {func(c *config.Config) { c.App.Environment = "development" }, "app.environment must be staging or test"},
		"empty environment":     {func(c *config.Config) { c.App.Environment = "" }, "app.environment must be staging or test"},
		"no migration URL":      {func(c *config.Config) { c.Database.MigrationURL = "" }, "database.migration_url and database.url are both required"},
		"blank runtime URL":     {func(c *config.Config) { c.Database.URL = "  " }, "database.migration_url and database.url are both required"},
		"same URL":              {func(c *config.Config) { c.Database.URL = c.Database.MigrationURL + " " }, "database.migration_url and database.url are the same"},
		"quoted runtime role":   {func(c *config.Config) { c.Database.RuntimeRole = `rereply"app` }, "database.runtime_role is not a plain PostgreSQL identifier"},
		"empty runtime role":    {func(c *config.Config) { c.Database.RuntimeRole = "" }, "database.runtime_role is not a plain PostgreSQL identifier"},
		"admin@admin.com":       {func(c *config.Config) { c.DefaultAdmin.Email = forbiddenAdminEmail }, "default_admin.email must not be admin@admin.com"},
		"ADMIN@admin.com":       {func(c *config.Config) { c.DefaultAdmin.Email = " ADMIN@Admin.com " }, "default_admin.email must not be admin@admin.com"},
		"example password":      {func(c *config.Config) { c.DefaultAdmin.Password = exampleAdminPassword }, "default_admin.password is config.example.toml's"},
		"15-character password": {func(c *config.Config) { c.DefaultAdmin.Password = strings.Repeat("x", 15) }, "default_admin.password is shorter than 16 characters"},
		"default password":      {func(c *config.Config) { c.DefaultAdmin.Password = "admin" }, "default_admin.password is shorter than 16 characters"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(cfg)
			err := checkConfig(cfg)
			require.Error(t, err)
			require.IsType(t, refusal{}, err)
			require.Equal(t, tc.reason, err.Error())
		})
	}
}

// The refused password and address are the repository's own: the example
// config's placeholder and the address the migration makes a super admin.
func TestRefusedAdminValuesMatchTheRepository(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config.example.toml"))
	require.NoError(t, err)
	example, err := toml.Parser().Unmarshal(raw)
	require.NoError(t, err)
	admin, ok := example["default_admin"].(map[string]any)
	require.True(t, ok, "config.example.toml has no [default_admin]")
	require.Equal(t, exampleAdminPassword, admin["password"])

	source, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "database", "postgres.go"))
	require.NoError(t, err)
	grants := regexp.MustCompile(`UPDATE users SET is_super_admin = true WHERE email = '([^']+)'`).FindAllStringSubmatch(string(source), -1)
	require.Len(t, grants, 1, "the migration's super-admin grant moved; review forbiddenAdminEmail")
	require.Equal(t, forbiddenAdminEmail, grants[0][1])
}

func writeConfig(t *testing.T, environment, runtimeURL, migrationURL, runtimeRole, adminPassword string) string {
	t.Helper()
	body := fmt.Sprintf(`[app]
environment = %q

[database]
url = %q
migration_url = %q
runtime_role = %q

[default_admin]
email = "staging-admin@example.com"
password = %q
full_name = "Synthetic Administrator"
`, environment, runtimeURL, migrationURL, runtimeRole, adminPassword)
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func runTool(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// Config refusals come before any connection: these URLs point at a closed
// port, so a connection attempt would report "cannot connect" instead.
func TestRunRefusesBeforeConnecting(t *testing.T) {
	closed := "postgres://owner:owner-password@127.0.0.1:1/rereply?sslmode=disable&connect_timeout=1"
	runtime := "postgres://rereply_app:runtime-password@127.0.0.1:1/rereply?sslmode=disable&connect_timeout=1"
	cases := map[string]struct {
		path   string
		reason string
	}{
		"production": {writeConfig(t, "production", runtime, closed, "rereply_app", testAdminPassword),
			"app.environment must be staging or test"},
		"example password": {writeConfig(t, "staging", runtime, closed, "rereply_app", exampleAdminPassword),
			"default_admin.password is config.example.toml's"},
		"no migration URL": {writeConfig(t, "test", runtime, "", "rereply_app", testAdminPassword),
			"database.migration_url and database.url are both required"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := runTool(t, "-config", tc.path)
			require.Equal(t, exitRefused, code)
			require.Empty(t, stdout)
			require.Equal(t, logPrefix+"refusing: "+tc.reason+"\n", stderr)
		})
	}
	code, _, stderr := runTool(t, "-config", writeConfig(t, "test", runtime, closed, "rereply_app", testAdminPassword), "extra")
	require.Equal(t, exitRefused, code)
	require.Contains(t, stderr, "refusing: unexpected arguments")

	code, _, stderr = runTool(t, "-config", writeConfig(t, "test", runtime, closed, "rereply_app", testAdminPassword))
	require.Equal(t, exitRefused, code)
	require.Equal(t, logPrefix+"refusing: cannot connect with database.migration_url\n", stderr)
}

// repositoryRoot is the module root, relative to this package.
var repositoryRoot = filepath.Join("..", "..", "..")

// TestBackfillsMatchRunRLSMigration keeps backfill() equal to the callback
// runRLSMigration passes to database.RunRLSMigrationCoordinator as its
// backfill parameter. It compares every call in the two, in source order, by
// import path and with literal arguments kept. Only logging and error
// construction are left out, so any new step in the callback, whatever its
// name, fails here until backfill() runs it too. The staging-bootstrap CI job
// cannot catch that drift: on the bootstrapped database rls-migrate takes the
// verify-only path and never runs the callback.
func TestBackfillsMatchRunRLSMigration(t *testing.T) {
	position := parameterIndex(t, filepath.Join(repositoryRoot, "internal", "database", "postgres.go"),
		"RunRLSMigrationCoordinator", "backfill")
	production := parseGoFile(t, filepath.Join(repositoryRoot, "cmd", "whatomate", "main.go"))
	callback := production.coordinatorArgument(t, "runRLSMigration", position)
	tool := parseGoFile(t, "main.go")
	require.Equal(t, production.steps(callback.Body), tool.steps(tool.function(t, "backfill").Body))
	require.Equal(t, []string{
		"github.com/shridarpatil/whatomate/internal/handlers.BackfillChatbotFlowGraph(_, _)",
		"github.com/shridarpatil/whatomate/internal/channel.BackfillLegacyWhatsAppInbox(_, 500)",
	}, tool.steps(tool.function(t, "backfill").Body))
}

type goFile struct {
	path    string
	file    *ast.File
	imports map[string]string // local name to import path
}

func parseGoFile(t *testing.T, path string) goFile {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	require.NoError(t, err)
	imports := map[string]string{}
	for _, spec := range parsed.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		require.NoError(t, err)
		name := importPath[strings.LastIndex(importPath, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = importPath
	}
	return goFile{path: path, file: parsed, imports: imports}
}

// function returns the one top-level function with this name.
func (f goFile) function(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()
	var found *ast.FuncDecl
	for _, declaration := range f.file.Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
			require.Nil(t, found, "%s declares %s twice", f.path, name)
			found = fn
		}
	}
	require.NotNil(t, found, "%s has no function %s", f.path, name)
	return found
}

// parameterIndex is the position of a named parameter of a function.
func parameterIndex(t *testing.T, path, function, parameter string) int {
	t.Helper()
	position := 0
	for _, field := range parseGoFile(t, path).function(t, function).Type.Params.List {
		for _, name := range field.Names {
			if name.Name == parameter {
				return position
			}
			position++
		}
	}
	t.Fatalf("%s: %s has no parameter %s", path, function, parameter)
	return 0
}

// coordinatorArgument is the function literal that the one
// database.RunRLSMigrationCoordinator call inside function passes at
// position.
func (f goFile) coordinatorArgument(t *testing.T, function string, position int) *ast.FuncLit {
	t.Helper()
	var calls []*ast.CallExpr
	ast.Inspect(f.function(t, function).Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && f.callee(call.Fun) ==
			"github.com/shridarpatil/whatomate/internal/database.RunRLSMigrationCoordinator" {
			calls = append(calls, call)
		}
		return true
	})
	require.Len(t, calls, 1, "%s: %s does not call database.RunRLSMigrationCoordinator exactly once", f.path, function)
	require.Greater(t, len(calls[0].Args), position)
	literal, ok := calls[0].Args[position].(*ast.FuncLit)
	require.True(t, ok, "%s: the coordinator's backfill argument is not a function literal; update this test", f.path)
	return literal
}

// callee names a called function: a package-qualified call by its import
// path, anything else as written.
func (f goFile) callee(fun ast.Expr) string {
	if selector, ok := fun.(*ast.SelectorExpr); ok {
		if qualifier, ok := selector.X.(*ast.Ident); ok {
			if importPath, ok := f.imports[qualifier.Name]; ok {
				return importPath + "." + selector.Sel.Name
			}
		}
	}
	return types.ExprString(fun)
}

var (
	// errorConstruction builds the returned error; it is not a step.
	errorConstruction = map[string]bool{"fmt.Errorf": true, "errors.New": true, "errors.Join": true}
	logLevels         = map[string]bool{"Debug": true, "Info": true, "Warn": true, "Error": true}
)

// logging reports a leveled call on a logger value, such as lo.Info(...).
func (f goFile) logging(fun ast.Expr) bool {
	selector, ok := fun.(*ast.SelectorExpr)
	if !ok || !logLevels[selector.Sel.Name] {
		return false
	}
	qualifier, ok := selector.X.(*ast.Ident)
	if !ok {
		return false
	}
	_, isPackage := f.imports[qualifier.Name]
	return !isPackage
}

// steps lists, in source order, every call in body except logging and error
// construction, with literal arguments kept and the others written as _.
func (f goFile) steps(body *ast.BlockStmt) []string {
	var steps []string
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee := f.callee(call.Fun)
		if errorConstruction[callee] || f.logging(call.Fun) {
			return true
		}
		arguments := make([]string, 0, len(call.Args))
		for _, argument := range call.Args {
			if literal, ok := argument.(*ast.BasicLit); ok {
				arguments = append(arguments, literal.Value)
			} else {
				arguments = append(arguments, "_")
			}
		}
		steps = append(steps, fmt.Sprintf("%s(%s)", callee, strings.Join(arguments, ", ")))
		return true
	})
	return steps
}

// ---------------------------------------------------------------------------
// Refusals that read the database. They need TEST_DATABASE_URL as a
// superuser, which plays DigitalOcean's control plane: it creates a
// doadmin-like owner (NOSUPERUSER CREATEDB CREATEROLE BYPASSRLS) that owns
// the database and creates the runtime role, so PostgreSQL 16+ makes it an
// implicit ADMIN member of that role, as in .github/workflows/test.yml's
// staging-bootstrap job.
// ---------------------------------------------------------------------------

type cluster struct {
	t      *testing.T
	admin  *gorm.DB
	base   *url.URL
	suffix string
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set, skipping database test")
	}
	base, err := url.Parse(dsn)
	require.NoError(t, err)
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := admin.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return &cluster{t: t, admin: admin, base: base, suffix: strings.ReplaceAll(uuid.NewString(), "-", "")[:12]}
}

func (c *cluster) exec(statements ...string) {
	c.t.Helper()
	for _, statement := range statements {
		require.NoError(c.t, c.admin.Exec(statement).Error, statement)
	}
}

// role creates a LOGIN role, as creator when creator is not empty (so that
// creator becomes an implicit ADMIN member of it), and drops it afterwards.
func (c *cluster) role(name, attributes, creator string) (string, string) {
	c.t.Helper()
	name = name + "_" + c.suffix
	password := strings.ReplaceAll(uuid.NewString(), "-", "")
	create := fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' %s", name, password, attributes)
	if creator == "" {
		c.exec(create)
	} else {
		require.NoError(c.t, c.admin.Connection(func(session *gorm.DB) error {
			for _, statement := range []string{"SET ROLE " + creator, create, "RESET ROLE"} {
				if err := session.Exec(statement).Error; err != nil {
					return err
				}
			}
			return nil
		}))
	}
	c.t.Cleanup(func() {
		_ = c.admin.Exec("DROP OWNED BY " + name).Error
		_ = c.admin.Exec("DROP ROLE IF EXISTS " + name).Error
	})
	return name, password
}

func (c *cluster) database(name, owner string) string {
	c.t.Helper()
	name = name + "_" + c.suffix
	c.exec("CREATE DATABASE " + name + " OWNER " + owner)
	c.t.Cleanup(func() { _ = c.admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)").Error })
	return name
}

func (c *cluster) url(role, password, databaseName string, extra ...string) string {
	target := *c.base
	target.User = url.UserPassword(role, password)
	target.Path = "/" + databaseName
	target.RawPath = ""
	query := target.Query()
	for index := 0; index+1 < len(extra); index += 2 {
		query.Set(extra[index], extra[index+1])
	}
	target.RawQuery = query.Encode()
	return target.String()
}

func (c *cluster) superuserURL(databaseName string) string {
	target := *c.base
	target.Path = "/" + databaseName
	target.RawPath = ""
	return target.String()
}

func (c *cluster) open(dsn string) *gorm.DB {
	c.t.Helper()
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(c.t, err)
	sqlDB, err := db.DB()
	require.NoError(c.t, err)
	c.t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func (c *cluster) publicObjects(databaseName string) publicSchema {
	c.t.Helper()
	objects, err := readPublicSchema(c.open(c.superuserURL(databaseName)))
	require.NoError(c.t, err)
	return objects
}

// doShape is DigitalOcean's role shape: a doadmin-like owner that owns an
// empty database and created the runtime role.
type doShape struct {
	*cluster
	owner, ownerPassword     string
	runtime, runtimePassword string
	database                 string
}

func newDOShape(t *testing.T) *doShape {
	c := newCluster(t)
	owner, ownerPassword := c.role("doadmin", "NOSUPERUSER CREATEDB CREATEROLE BYPASSRLS", "")
	runtime, runtimePassword := c.role("rereply_app", "NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS", owner)
	databaseName := c.database("rereply_bootstrap", owner)
	var implicit int64
	require.NoError(t, c.admin.Raw(`
		SELECT COUNT(*) FROM pg_catalog.pg_auth_members
		WHERE roleid = ?::regrole AND member = ?::regrole AND admin_option
	`, runtime, owner).Scan(&implicit).Error)
	require.EqualValues(t, 1, implicit, "PostgreSQL did not make the creator an ADMIN member of the runtime role")
	return &doShape{cluster: c, owner: owner, ownerPassword: ownerPassword,
		runtime: runtime, runtimePassword: runtimePassword, database: databaseName}
}

func (s *doShape) ownerURL(extra ...string) string {
	return s.url(s.owner, s.ownerPassword, s.database, extra...)
}

func (s *doShape) runtimeURL(extra ...string) string {
	return s.url(s.runtime, s.runtimePassword, s.database, extra...)
}

// refused runs the tool and requires a refusal with nothing written.
func (s *doShape) refused(migrationURL, runtimeURL, runtimeRole, reason string) {
	s.t.Helper()
	before := s.publicObjects(s.database)
	code, stdout, stderr := runTool(s.t, "-config", writeConfig(s.t, "test", runtimeURL, migrationURL, runtimeRole, testAdminPassword))
	require.Equal(s.t, exitRefused, code, stderr)
	require.Empty(s.t, stdout)
	require.Equal(s.t, logPrefix+"refusing: "+reason+"\n", stderr)
	require.Equal(s.t, before, s.publicObjects(s.database), "a refused run wrote to the public schema")
	for _, secret := range []string{s.ownerPassword, s.runtimePassword, testAdminPassword} {
		require.NotContains(s.t, stderr, secret)
	}
}

func TestPreflightAcceptsTheDigitalOceanRoleShape(t *testing.T) {
	shape := newDOShape(t)
	owner := shape.open(shape.ownerURL())
	runtime := shape.open(shape.runtimeURL())
	require.NoError(t, preflight(owner, runtime, shape.runtime))
	require.Equal(t, publicSchema{Schemas: 1}, shape.publicObjects(shape.database))
}

func TestMembershipPreflight(t *testing.T) {
	t.Run("an owner role created by doadmin has doadmin as an implicit ADMIN member", func(t *testing.T) {
		shape := newDOShape(t)
		created, createdPassword := shape.role("rereply_owner", "NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS", shape.owner)
		shape.database = shape.cluster.database("rereply_negative", created)
		shape.refused(shape.url(created, createdPassword, shape.database), shape.runtimeURL(), shape.runtime,
			"membership preflight: roles other than the migration owner are members of the owner=1 runtime=1")
	})
	t.Run("another member of the owner", func(t *testing.T) {
		shape := newDOShape(t)
		member, _ := shape.role("owner_member", "NOSUPERUSER", "")
		shape.exec("GRANT " + shape.owner + " TO " + member)
		shape.refused(shape.ownerURL(), shape.runtimeURL(), shape.runtime,
			"membership preflight: roles other than the migration owner are members of the owner=1 runtime=1")
	})
	t.Run("a transitive member of the owner", func(t *testing.T) {
		shape := newDOShape(t)
		outer, _ := shape.role("owner_outer", "NOSUPERUSER", "")
		inner, _ := shape.role("owner_inner", "NOSUPERUSER", "")
		shape.exec("GRANT "+shape.owner+" TO "+outer, "GRANT "+outer+" TO "+inner)
		shape.refused(shape.ownerURL(), shape.runtimeURL(), shape.runtime,
			"membership preflight: roles other than the migration owner are members of the owner=2 runtime=2")
	})
	t.Run("another member of the runtime role", func(t *testing.T) {
		shape := newDOShape(t)
		member, _ := shape.role("runtime_member", "NOSUPERUSER", "")
		shape.exec("GRANT " + shape.runtime + " TO " + member)
		shape.refused(shape.ownerURL(), shape.runtimeURL(), shape.runtime,
			"membership preflight: roles other than the migration owner are members of the owner=0 runtime=1")
	})
	t.Run("the runtime role as a member of the owner", func(t *testing.T) {
		// An owner without CREATEROLE or BYPASSRLS, so the privileged-role
		// gate does not see the membership first.
		c := newCluster(t)
		owner, ownerPassword := c.role("plain_owner", "NOSUPERUSER", "")
		runtime, runtimePassword := c.role("rereply_app", "NOSUPERUSER", "")
		c.exec("GRANT " + owner + " TO " + runtime)
		shape := &doShape{cluster: c, owner: owner, ownerPassword: ownerPassword, runtime: runtime,
			runtimePassword: runtimePassword, database: c.database("rereply_bootstrap", owner)}
		shape.refused(shape.ownerURL(), shape.runtimeURL(), shape.runtime,
			"membership preflight: roles other than the migration owner are members of the owner=1 runtime=0")
	})
	t.Run("the runtime role inside a privileged owner", func(t *testing.T) {
		c := newCluster(t)
		owner, ownerPassword := c.role("doadmin", "NOSUPERUSER CREATEDB CREATEROLE BYPASSRLS", "")
		runtime, runtimePassword := c.role("rereply_app", "NOSUPERUSER", "")
		c.exec("GRANT " + owner + " TO " + runtime)
		shape := &doShape{cluster: c, owner: owner, ownerPassword: ownerPassword, runtime: runtime,
			runtimePassword: runtimePassword, database: c.database("rereply_bootstrap", owner)}
		shape.refused(shape.ownerURL(), shape.runtimeURL(), shape.runtime,
			"the runtime role is a member of privileged roles=1")
	})
}

func TestRoleRefusals(t *testing.T) {
	t.Run("superuser owner", func(t *testing.T) {
		shape := newDOShape(t)
		shape.refused(shape.superuserURL(shape.database), shape.runtimeURL(), shape.runtime,
			"the migration owner is a superuser")
	})
	for attribute, reason := range map[string]string{
		"SUPERUSER":   "the runtime role must be NOSUPERUSER NOCREATEROLE NOBYPASSRLS NOREPLICATION",
		"BYPASSRLS":   "the runtime role must be NOSUPERUSER NOCREATEROLE NOBYPASSRLS NOREPLICATION",
		"CREATEROLE":  "the runtime role must be NOSUPERUSER NOCREATEROLE NOBYPASSRLS NOREPLICATION",
		"REPLICATION": "the runtime role must be NOSUPERUSER NOCREATEROLE NOBYPASSRLS NOREPLICATION",
	} {
		t.Run("runtime role with "+attribute, func(t *testing.T) {
			shape := newDOShape(t)
			shape.exec("ALTER ROLE " + shape.runtime + " " + attribute)
			shape.refused(shape.ownerURL(), shape.runtimeURL(), shape.runtime, reason)
		})
	}
	t.Run("runtime role inside a BYPASSRLS role", func(t *testing.T) {
		shape := newDOShape(t)
		privileged, _ := shape.role("privileged", "NOSUPERUSER BYPASSRLS", "")
		shape.exec("GRANT " + privileged + " TO " + shape.runtime)
		shape.refused(shape.ownerURL(), shape.runtimeURL(), shape.runtime,
			"the runtime role is a member of privileged roles=1")
	})
}

func TestIdentityRefusals(t *testing.T) {
	t.Run("both URLs connect as the owner", func(t *testing.T) {
		shape := newDOShape(t)
		shape.refused(shape.ownerURL(), shape.ownerURL("application_name", "runtime"), shape.owner,
			"database.migration_url and database.url connect as the same role")
	})
	t.Run("the owner URL switches role at connection", func(t *testing.T) {
		shape := newDOShape(t)
		switched, _ := shape.role("switched", "NOSUPERUSER", "")
		shape.exec("GRANT " + switched + " TO " + shape.owner)
		shape.refused(shape.ownerURL("role", switched), shape.runtimeURL(), shape.runtime,
			"database.migration_url's current_user differs from its session_user")
	})
	t.Run("the runtime URL switches role at connection", func(t *testing.T) {
		shape := newDOShape(t)
		switched, _ := shape.role("switched", "NOSUPERUSER", "")
		shape.exec("GRANT " + switched + " TO " + shape.runtime)
		shape.refused(shape.ownerURL(), shape.runtimeURL("role", switched), shape.runtime,
			"database.url's current_user differs from its session_user")
	})
	t.Run("the runtime URL is not database.runtime_role", func(t *testing.T) {
		shape := newDOShape(t)
		other, _ := shape.role("other_runtime", "NOSUPERUSER", "")
		shape.refused(shape.ownerURL(), shape.runtimeURL(), other,
			"database.url does not connect as database.runtime_role")
	})
	t.Run("the URLs name different databases", func(t *testing.T) {
		shape := newDOShape(t)
		elsewhere := shape.cluster.database("rereply_elsewhere", shape.owner)
		shape.refused(shape.ownerURL(), shape.url(shape.runtime, shape.runtimePassword, elsewhere), shape.runtime,
			"database.migration_url and database.url name different databases")
	})
	// A missing database, not a wrong password: the postgres image trusts
	// loopback connections, so a password test would depend on pg_hba.
	t.Run("a failed connection names only the SQLSTATE", func(t *testing.T) {
		shape := newDOShape(t)
		shape.refused(shape.url(shape.owner, shape.ownerPassword, "missing_"+shape.suffix), shape.runtimeURL(), shape.runtime,
			"cannot connect with database.migration_url (SQLSTATE 3D000)")
	})
}

func TestPublicSchemaRefusals(t *testing.T) {
	for kind, statement := range map[string]string{
		"relations=1 functions=0 types=0": "CREATE SEQUENCE public.leftover",
		"relations=0 functions=1 types=0": "CREATE FUNCTION public.leftover() RETURNS integer LANGUAGE sql AS 'SELECT 1'",
		// A domain and its array type.
		"relations=0 functions=0 types=2": "CREATE DOMAIN public.leftover AS integer",
	} {
		t.Run(kind, func(t *testing.T) {
			shape := newDOShape(t)
			require.NoError(t, shape.open(shape.ownerURL()).Exec(statement).Error)
			shape.refused(shape.ownerURL(), shape.runtimeURL(), shape.runtime, "public schema is not empty "+kind)
		})
	}
	t.Run("missing public schema", func(t *testing.T) {
		shape := newDOShape(t)
		require.NoError(t, shape.open(shape.superuserURL(shape.database)).Exec("DROP SCHEMA public").Error)
		shape.refused(shape.ownerURL(), shape.runtimeURL(), shape.runtime, "the public schema is missing")
	})
}

func TestConcurrentBootstrapIsRefused(t *testing.T) {
	shape := newDOShape(t)
	holder := shape.open(shape.ownerURL())
	sqlDB, err := holder.DB()
	require.NoError(t, err)
	conn, err := sqlDB.Conn(context.Background())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	var acquired bool
	require.NoError(t, conn.QueryRowContext(context.Background(),
		"SELECT pg_catalog.pg_try_advisory_lock(pg_catalog.hashtextextended($1, 0))", lockName).Scan(&acquired))
	require.True(t, acquired)
	shape.refused(shape.ownerURL(), shape.runtimeURL(), shape.runtime,
		"another staging bootstrap is running against this database")
	unlockOnce(t, conn)
}

func unlockOnce(t *testing.T, conn *sql.Conn) {
	t.Helper()
	var released bool
	require.NoError(t, conn.QueryRowContext(context.Background(),
		"SELECT pg_catalog.pg_advisory_unlock(pg_catalog.hashtextextended($1, 0))", lockName).Scan(&released))
	require.True(t, released)
}
