package database_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/dbcatalog"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const catalogGoldenDirectory = "../dbcatalog/golden"

// This discovery exists only in the synthetic test exporter. The runtime reader
// never learns its equality allowlist from the database it is inspecting.
func discoverSyntheticCatalog(t *testing.T, db *gorm.DB) dbcatalog.Catalog {
	t.Helper()
	// Match Capture's deparser context, including arguments of custom types.
	// The setting is local to this discovery transaction and never leaks into
	// the migration connection used by the rest of the synthetic test.
	tx := db.Begin()
	require.NoError(t, tx.Error)
	defer tx.Rollback()
	require.NoError(t, tx.Exec(`SET LOCAL search_path=pg_catalog`).Error)
	db = tx
	queries := []struct{ kind, sql string }{
		{"relations", `SELECT c.relname AS name, '' AS parent FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p','v','m','f')`},
		{"columns", `SELECT a.attname AS name,c.relname AS parent FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p','v','m','f') AND a.attnum>0 AND NOT a.attisdropped`},
		{"constraints", `SELECT co.conname AS name,COALESCE(c.relname,ty.typname) AS parent FROM pg_constraint co JOIN pg_namespace n ON n.oid=co.connamespace LEFT JOIN pg_class c ON c.oid=co.conrelid LEFT JOIN pg_type ty ON ty.oid=co.contypid WHERE n.nspname='public'`},
		{"indexes", `SELECT i.relname AS name,c.relname AS parent FROM pg_index x JOIN pg_class i ON i.oid=x.indexrelid JOIN pg_class c ON c.oid=x.indrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'`},
		{"triggers", `SELECT tg.tgname AS name,c.relname AS parent FROM pg_trigger tg JOIN pg_class c ON c.oid=tg.tgrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND NOT tg.tgisinternal`},
		{"functions", `SELECT p.proname AS name,pg_get_function_identity_arguments(p.oid) AS parent FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public'`},
		{"policies", `SELECT p.polname AS name,c.relname AS parent FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'`},
		{"sequences", `SELECT c.relname AS name,'' AS parent FROM pg_sequence s JOIN pg_class c ON c.oid=s.seqrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'`},
		{"types", `SELECT ty.typname AS name,'' AS parent FROM pg_type ty JOIN pg_namespace n ON n.oid=ty.typnamespace WHERE n.nspname='public'`},
		{"extensions", `SELECT e.extname AS name,'' AS parent FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE n.nspname='public'`},
	}
	catalog := dbcatalog.Catalog{Version: 0, Objects: []dbcatalog.Object{}}
	for _, query := range queries {
		var rows []struct{ Name, Parent string }
		require.NoError(t, db.Raw(query.sql).Scan(&rows).Error)
		for _, row := range rows {
			identity, parent := row.Name, row.Parent
			if query.kind == "functions" {
				hash := sha256.Sum256([]byte(parent))
				identity, parent = identity+"("+hex.EncodeToString(hash[:])+")", ""
			} else if parent != "" {
				identity = parent + "." + identity
			}
			catalog.Objects = append(catalog.Objects, dbcatalog.Object{Kind: query.kind, Identity: identity, Parent: parent, Attributes: map[string]string{}})
		}
	}
	return catalog
}

func captureSyntheticCatalog(t *testing.T, f catalogFixture, known dbcatalog.Catalog, globals []dbcatalog.GlobalTable) dbcatalog.Snapshot {
	t.Helper()
	snapshot, err := dbcatalog.Capture(context.Background(), f.runtimeDSN, dbcatalog.Options{Catalog: known, GlobalTables: globals, IncludeSeeds: true, Timeout: 45 * time.Second})
	require.NoError(t, err)
	return snapshot
}

func loadCatalogGoldens(t *testing.T) (dbcatalog.Catalog, []dbcatalog.GlobalTable) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(catalogGoldenDirectory, "catalog.json"))
	require.NoError(t, err)
	known, err := dbcatalog.LoadCatalog(raw)
	require.NoError(t, err)
	raw, err = os.ReadFile(filepath.Join(catalogGoldenDirectory, "global_tables.json"))
	require.NoError(t, err)
	globals, err := dbcatalog.LoadGlobalTables(raw)
	require.NoError(t, err)
	return known, globals
}

func TestTenantRLS_CatalogGoldenMatchesBootstrap(t *testing.T) {
	f := catalogClone(t)
	var known dbcatalog.Catalog
	var globals []dbcatalog.GlobalTable
	if os.Getenv("UPDATE_CATALOG_GOLDEN") == "1" {
		known = discoverSyntheticCatalog(t, f.owner)
		globals = []dbcatalog.GlobalTable{{Name: "permissions", Reason: "Code-defined global resource/action permission vocabulary; models.Permission and SeedPermissionsAndRoles", Keys: []string{"resource", "action"}}}
	} else {
		known, globals = loadCatalogGoldens(t)
	}
	snapshot := captureSyntheticCatalog(t, f, known, globals)
	require.Empty(t, snapshot.Unknown)
	for _, object := range snapshot.Objects {
		require.False(t, object.Missing, object.Identity)
	}
	canonical, err := dbcatalog.CanonicalCatalog(dbcatalog.Catalog{Version: 0, Objects: snapshot.Objects})
	require.NoError(t, err)
	if os.Getenv("UPDATE_CATALOG_GOLDEN") == "1" {
		// Explicit maintainer-only regeneration from the disposable bootstrap,
		// never an automatic response to a comparator mismatch.
		require.NoError(t, os.MkdirAll(catalogGoldenDirectory, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(catalogGoldenDirectory, "catalog.json"), canonical, 0644))
		if _, err := os.Stat(filepath.Join(catalogGoldenDirectory, "global_tables.json")); os.IsNotExist(err) {
			encoded, err := json.MarshalIndent(globals, "", "  ")
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(catalogGoldenDirectory, "global_tables.json"), append(encoded, '\n'), 0644))
		}
		require.NoError(t, os.WriteFile(filepath.Join(catalogGoldenDirectory, "history.json"), []byte("[{\"version\":0,\"class\":\"baseline\",\"min_reader_version\":0}]\n"), 0644))
		t.Logf("synthetic catalog: objects=%d sha256=%s", len(snapshot.Objects), snapshot.SHA256)
		for _, object := range snapshot.Objects {
			if object.Kind == "relations" && (object.Attributes["rls"] != "true" || object.Attributes["force_rls"] != "true") {
				t.Logf("synthetic relation requiring explicit global reason: %s", object.Identity)
			}
		}
		return
	}
	differences, err := dbcatalog.Compare(snapshot, known)
	require.NoError(t, err)
	require.Empty(t, differences)
	raw, err := os.ReadFile(filepath.Join(catalogGoldenDirectory, "catalog.json"))
	require.NoError(t, err)
	require.Equal(t, raw, canonical)
	require.Len(t, snapshot.Seeds, 1)
	require.Equal(t, "permissions", snapshot.Seeds[0].Table)
	// The keys are code-owned public vocabulary, not generated row IDs.
	require.Positive(t, snapshot.Seeds[0].Count)
}

func TestTenantRLS_CatalogSnapshotIsReadOnlyAndRoleSymbolic(t *testing.T) {
	f := catalogClone(t)
	known, globals := loadCatalogGoldens(t)
	before := captureSyntheticCatalog(t, f, known, globals)
	u, err := catalogTestURL(f.runtimeDSN)
	require.NoError(t, err)
	q := u.Query()
	q.Set("default_transaction_read_only", "on")
	u.RawQuery = q.Encode()
	f.runtimeDSN = u.String()
	after := captureSyntheticCatalog(t, f, known, globals)
	require.Equal(t, before, after)
	var output bytes.Buffer
	require.NoError(t, dbcatalog.WritePublic(&output, after, known, "json"))
	require.NotContains(t, output.String(), catalogTemplate.owner)
	require.NotContains(t, output.String(), catalogTemplate.runtime)
	require.Contains(t, output.String(), "MIGRATION_OWNER")
	require.Contains(t, output.String(), "RUNTIME")
	require.True(t, after.Ownership.MigrationOwnerOwnsDatabase)
	require.True(t, after.Ownership.MigrationOwnerOwnsPublicSchema)
	require.NoError(t, database.VerifyTenantRLS(f.runtime, catalogTemplate.runtime))
}

func TestTenantRLS_CatalogUnknownObjectsCountOnlyAndKnownMissing(t *testing.T) {
	f := catalogClone(t)
	known, globals := loadCatalogGoldens(t)
	before := captureSyntheticCatalog(t, f, known, globals)
	require.NoError(t, f.owner.Exec(`CREATE TABLE public.synthetic_unknown_private_name (id integer); CREATE SCHEMA synthetic_nonpublic_private_name; CREATE TABLE synthetic_nonpublic_private_name.synthetic_private_table (id integer)`).Error)
	after := captureSyntheticCatalog(t, f, known, globals)
	require.Equal(t, before.SHA256, after.SHA256)
	require.NotEmpty(t, after.Unknown)
	require.Greater(t, after.CountOnly["non_public_schemas"], before.CountOnly["non_public_schemas"])
	var output bytes.Buffer
	require.NoError(t, dbcatalog.WritePublic(&output, after, known, "json"))
	require.NotContains(t, output.String(), "synthetic_unknown_private_name")
	require.NotContains(t, output.String(), "synthetic_nonpublic_private_name")
	require.NotContains(t, output.String(), "synthetic_private_table")
	// A known identity disappears only in this disposable clone.
	require.NoError(t, f.owner.Exec(`ALTER TABLE public.permissions RENAME COLUMN description TO synthetic_unknown_description`).Error)
	missing := captureSyntheticCatalog(t, f, known, globals)
	require.NotEqual(t, before.SHA256, missing.SHA256)
	found := false
	for _, object := range missing.Objects {
		if object.Identity == "permissions.description" {
			found = object.Missing
		}
	}
	require.True(t, found)
}

func TestTenantRLS_CatalogSeedKeysExcludeRowValues(t *testing.T) {
	f := catalogClone(t)
	known, globals := loadCatalogGoldens(t)
	before := captureSyntheticCatalog(t, f, known, globals)
	require.NoError(t, f.owner.Exec(`UPDATE public.permissions SET description='synthetic-private-description@example.invalid'`).Error)
	after := captureSyntheticCatalog(t, f, known, globals)
	require.Equal(t, before.Seeds, after.Seeds)
	require.Equal(t, before.SHA256, after.SHA256)
	var output bytes.Buffer
	require.NoError(t, dbcatalog.WritePublic(&output, after, known, "json"))
	require.NotContains(t, output.String(), "synthetic-private-description")
	require.NoError(t, f.owner.Exec(`INSERT INTO public.permissions(resource,action,description) VALUES ('synthetic.catalog','read','synthetic-private-description@example.invalid')`).Error)
	changed := captureSyntheticCatalog(t, f, known, globals)
	require.Equal(t, before.Seeds[0].Count+1, changed.Seeds[0].Count)
	require.NotEqual(t, before.Seeds[0].KeysSHA256, changed.Seeds[0].KeysSHA256)
	require.Equal(t, before.SHA256, changed.SHA256)
}

func TestTenantRLS_CatalogAdditionalShapes(t *testing.T) {
	f := catalogClone(t)
	baseline, globals := loadCatalogGoldens(t)
	before := captureSyntheticCatalog(t, f, baseline, globals)
	require.NoError(t, f.owner.Exec(`
CREATE SEQUENCE public.synthetic_shape_sequence;
CREATE TYPE public.synthetic_shape_enum AS ENUM ('first','second');
CREATE TYPE public.synthetic_shape_composite AS (first integer);
CREATE TYPE public.synthetic_shape_range AS RANGE (subtype=integer);
CREATE TABLE public.synthetic_shape_partitioned (id integer, bucket integer) PARTITION BY RANGE (id);
CREATE TABLE public.synthetic_shape_partition PARTITION OF public.synthetic_shape_partitioned FOR VALUES FROM (0) TO (10);
CREATE TABLE public.synthetic_shape_indexed (id integer, bucket integer, payload text);
CREATE INDEX synthetic_shape_index ON public.synthetic_shape_indexed (id) INCLUDE (bucket) WHERE id > 0;
CREATE TABLE public.synthetic_shape_constrained (id integer CONSTRAINT synthetic_shape_unique UNIQUE WITH (fillfactor=80));
CREATE TABLE public.synthetic_shape_excluded (value text, CONSTRAINT synthetic_shape_exclusion EXCLUDE USING btree (value WITH =) WITH (fillfactor=80));
`).Error)
	// Use only a trusted extension already shipped by the local PG image, with
	// no dependency installation or network access. Its absence is not a skip
	// of the required sequence/type/partition coverage.
	var trustedExtension int64
	require.NoError(t, f.owner.Raw(`SELECT count(*) FROM pg_available_extensions e JOIN pg_available_extension_versions v ON v.name=e.name AND v.version=e.default_version WHERE e.name='hstore' AND e.installed_version IS NULL AND v.trusted AND COALESCE(cardinality(v.requires),0)=0`).Scan(&trustedExtension).Error)
	if trustedExtension == 1 {
		require.NoError(t, f.owner.Exec(`CREATE EXTENSION hstore WITH SCHEMA public`).Error)
	}
	unknown := captureSyntheticCatalog(t, f, baseline, globals)
	require.Equal(t, before.SHA256, unknown.SHA256)
	require.NotEmpty(t, unknown.Unknown)
	var public bytes.Buffer
	require.NoError(t, dbcatalog.WritePublic(&public, unknown, baseline, "json"))
	require.NotContains(t, public.String(), "synthetic_shape_")
	if trustedExtension == 1 {
		require.NotContains(t, public.String(), "hstore")
	}

	known := discoverSyntheticCatalog(t, f.owner)
	initial := captureSyntheticCatalog(t, f, known, globals)
	require.Empty(t, initial.Unknown)
	for _, object := range initial.Objects {
		require.False(t, object.Missing, object.Identity)
	}
	object := func(snapshot dbcatalog.Snapshot, kind, identity string) dbcatalog.Object {
		t.Helper()
		for _, candidate := range snapshot.Objects {
			if candidate.Kind == kind && candidate.Identity == identity {
				return candidate
			}
		}
		t.Fatalf("synthetic %s object missing: %s", kind, identity)
		return dbcatalog.Object{}
	}
	changedAttribute := func(after dbcatalog.Snapshot, kind, identity, key string) {
		t.Helper()
		previous := object(initial, kind, identity)
		current := object(after, kind, identity)
		require.False(t, current.Missing)
		require.NotEmpty(t, previous.Attributes[key], key)
		require.NotEmpty(t, current.Attributes[key], key)
		require.NotEqual(t, previous.Attributes[key], current.Attributes[key], identity+":"+key)
	}
	// Storage settings are intentionally count-only, including settings on the
	// index that backs a UNIQUE constraint. Logical index structure is not.
	require.NoError(t, f.owner.Exec(`ALTER INDEX public.synthetic_shape_index SET (fillfactor=75); ALTER INDEX public.synthetic_shape_unique SET (fillfactor=90); ALTER INDEX public.synthetic_shape_exclusion SET (fillfactor=90)`).Error)
	storageOnly := captureSyntheticCatalog(t, f, known, globals)
	require.Equal(t, initial.SHA256, storageOnly.SHA256)
	require.Greater(t, storageOnly.CountOnly["reloptions"], initial.CountOnly["reloptions"])
	previousIndex := object(storageOnly, "indexes", "synthetic_shape_indexed.synthetic_shape_index").Attributes["definition_sha256"]
	for _, statement := range []string{
		`CREATE INDEX synthetic_shape_index ON public.synthetic_shape_indexed (bucket) INCLUDE (id) WHERE id > 0`,
		`CREATE INDEX synthetic_shape_index ON public.synthetic_shape_indexed (bucket) INCLUDE (payload) WHERE id > 0`,
		`CREATE INDEX synthetic_shape_index ON public.synthetic_shape_indexed (bucket) INCLUDE (payload) WHERE id > 1`,
	} {
		require.NoError(t, f.owner.Exec(`DROP INDEX public.synthetic_shape_index; `+statement).Error)
		semantic := captureSyntheticCatalog(t, f, known, globals)
		currentIndex := object(semantic, "indexes", "synthetic_shape_indexed.synthetic_shape_index").Attributes["definition_sha256"]
		require.NotEqual(t, previousIndex, currentIndex)
		previousIndex = currentIndex
	}
	require.NoError(t, f.owner.Exec(`ALTER TABLE public.synthetic_shape_excluded DROP CONSTRAINT synthetic_shape_exclusion; ALTER TABLE public.synthetic_shape_excluded ADD CONSTRAINT synthetic_shape_exclusion EXCLUDE USING btree (value WITH =) WHERE (value <> '')`).Error)
	exclusionPredicate := captureSyntheticCatalog(t, f, known, globals)
	changedAttribute(exclusionPredicate, "constraints", "synthetic_shape_excluded.synthetic_shape_exclusion", "definition_sha256")
	require.NoError(t, f.owner.Exec(`
ALTER SEQUENCE public.synthetic_shape_sequence INCREMENT BY 3 OWNED BY public.synthetic_shape_partitioned.id;
ALTER TYPE public.synthetic_shape_enum ADD VALUE 'third';
ALTER TYPE public.synthetic_shape_composite ADD ATTRIBUTE second text;
DROP TYPE public.synthetic_shape_range;
CREATE TYPE public.synthetic_shape_range AS RANGE (subtype=numeric);
ALTER TABLE public.synthetic_shape_partitioned DETACH PARTITION public.synthetic_shape_partition;
`).Error)
	detached := captureSyntheticCatalog(t, f, known, globals)
	changedAttribute(detached, "sequences", "synthetic_shape_sequence", "increment")
	changedAttribute(detached, "sequences", "synthetic_shape_sequence", "owned_by_sha256")
	changedAttribute(detached, "types", "synthetic_shape_enum", "enum_sha256")
	changedAttribute(detached, "types", "synthetic_shape_composite", "definition_sha256")
	changedAttribute(detached, "types", "synthetic_shape_range", "definition_sha256")
	changedAttribute(detached, "relations", "synthetic_shape_partition", "parents_sha256")
	changedAttribute(detached, "relations", "synthetic_shape_partition", "partition_bound_sha256")
	require.NoError(t, f.owner.Exec(`
ALTER TABLE public.synthetic_shape_partitioned ATTACH PARTITION public.synthetic_shape_partition FOR VALUES FROM (10) TO (20);
ALTER SEQUENCE public.synthetic_shape_sequence OWNED BY NONE;
ALTER TABLE public.synthetic_shape_partitioned DETACH PARTITION public.synthetic_shape_partition;
DROP TABLE public.synthetic_shape_partitioned;
CREATE TABLE public.synthetic_shape_partitioned (id integer, bucket integer) PARTITION BY RANGE (bucket);
ALTER TABLE public.synthetic_shape_partitioned ATTACH PARTITION public.synthetic_shape_partition FOR VALUES FROM (10) TO (20);
`).Error)
	changed := captureSyntheticCatalog(t, f, known, globals)
	changedAttribute(changed, "relations", "synthetic_shape_partitioned", "partition_key_sha256")
	changedAttribute(changed, "relations", "synthetic_shape_partition", "partition_bound_sha256")
	require.NotEqual(t, initial.SHA256, changed.SHA256)
	if trustedExtension == 1 {
		require.False(t, object(initial, "extensions", "hstore").Missing)
		require.NoError(t, f.owner.Exec(`DROP EXTENSION hstore`).Error)
		removed := captureSyntheticCatalog(t, f, known, globals)
		require.True(t, object(removed, "extensions", "hstore").Missing)
		require.NotEqual(t, changed.SHA256, removed.SHA256)
		t.Log("locally available trusted extension catalog branch exercised")
	} else {
		t.Log("no locally available dependency-free trusted hstore extension; required shape branches exercised")
	}
	// Even changed unknown definitions stay outside baseline equality/output.
	finalUnknown := captureSyntheticCatalog(t, f, baseline, globals)
	require.Equal(t, before.SHA256, finalUnknown.SHA256)
	public.Reset()
	require.NoError(t, dbcatalog.WritePublic(&public, finalUnknown, baseline, "json"))
	require.NotContains(t, public.String(), "synthetic_shape_")
}

// Relation security is evaluated from actual pg_catalog rows, independently of
// golden equality. An exporter cannot silently bless a new unprotected table.
func catalogRelationViolations(t *testing.T, db *gorm.DB, globals []dbcatalog.GlobalTable) []string {
	t.Helper()
	allowed := map[string]bool{}
	for _, global := range globals {
		require.NotEmpty(t, strings.TrimSpace(global.Reason))
		allowed[global.Name] = true
	}
	var rows []struct {
		Name, Kind      string
		Forced, Invoker bool
	}
	require.NoError(t, db.Raw(`SELECT c.relname AS name,c.relkind::text AS kind,c.relrowsecurity AND c.relforcerowsecurity AS forced,COALESCE('security_invoker=true'=ANY(c.reloptions),false) AS invoker FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p','v','m','f')`).Scan(&rows).Error)
	violations := []string{}
	for _, row := range rows {
		if allowed[row.Name] {
			continue
		}
		if (row.Kind == "r" || row.Kind == "p") && row.Forced {
			continue
		}
		if row.Kind == "v" && row.Invoker {
			var unsafe int64
			require.NoError(t, db.Raw(`SELECT count(*) FROM pg_rewrite rw JOIN pg_class v ON v.oid=rw.ev_class JOIN pg_namespace vn ON vn.oid=v.relnamespace JOIN pg_depend d ON d.classid='pg_rewrite'::regclass AND d.objid=rw.oid AND d.refclassid='pg_class'::regclass JOIN pg_class c ON c.oid=d.refobjid WHERE vn.nspname='public' AND v.relname=? AND c.oid<>v.oid AND NOT(c.relkind IN ('r','p') AND c.relrowsecurity AND c.relforcerowsecurity)`, row.Name).Scan(&unsafe).Error)
			if unsafe == 0 {
				continue
			}
		}
		violations = append(violations, row.Name)
	}
	var fks []struct{ Child, Parent string }
	require.NoError(t, db.Raw(`SELECT c.relname AS child,p.relname AS parent FROM pg_constraint co JOIN pg_class c ON c.oid=co.conrelid JOIN pg_namespace cn ON cn.oid=c.relnamespace JOIN pg_class p ON p.oid=co.confrelid JOIN pg_namespace pn ON pn.oid=p.relnamespace WHERE co.contype='f' AND cn.nspname='public' AND pn.nspname='public'`).Scan(&fks).Error)
	tenant := map[string]bool{}
	for _, name := range database.DirectTenantTables {
		tenant[name] = true
	}
	for name := range database.RelatedTenantTables {
		tenant[name] = true
	}
	for _, fk := range fks {
		if allowed[fk.Child] && tenant[fk.Parent] {
			violations = append(violations, fk.Child+"->tenant")
		}
	}
	sort.Strings(violations)
	return violations
}

func TestTenantRLS_CatalogEveryPublicRelationIsForcedOrGlobal(t *testing.T) {
	f := catalogClone(t)
	_, globals := loadCatalogGoldens(t)
	require.Empty(t, catalogRelationViolations(t, f.owner, globals))
	require.NoError(t, f.owner.Exec(`CREATE TABLE public.synthetic_unprotected (id integer); CREATE TABLE public.synthetic_partitioned(id integer) PARTITION BY RANGE(id); CREATE VIEW public.synthetic_definer_view AS SELECT id FROM public.contacts; CREATE VIEW public.synthetic_invoker_view WITH (security_invoker=true) AS SELECT id FROM public.contacts; CREATE VIEW public.synthetic_invoker_unsafe WITH (security_invoker=true) AS SELECT id FROM public.synthetic_unprotected; CREATE MATERIALIZED VIEW public.synthetic_materialized AS SELECT 1 AS id; CREATE TABLE public.synthetic_global_child (id uuid REFERENCES public.contacts(id))`).Error)
	withHostileGlobal := append(append([]dbcatalog.GlobalTable{}, globals...), dbcatalog.GlobalTable{Name: "synthetic_global_child", Reason: "Synthetic attempted tenant-child exemption"})
	violations := catalogRelationViolations(t, f.owner, withHostileGlobal)
	require.Contains(t, violations, "synthetic_unprotected")
	require.Contains(t, violations, "synthetic_partitioned")
	require.Contains(t, violations, "synthetic_invoker_unsafe")
	require.Contains(t, violations, "synthetic_definer_view")
	require.Contains(t, violations, "synthetic_materialized")
	require.Contains(t, violations, "synthetic_global_child->tenant")
	require.NotContains(t, violations, "synthetic_invoker_view")
}

func publicDefinerFunctions(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	var names []string
	require.NoError(t, db.Raw(`SELECT p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace CROSS JOIN LATERAL aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) a WHERE n.nspname='public' AND p.prosecdef AND a.grantee=0 AND a.privilege_type='EXECUTE' ORDER BY p.proname`).Scan(&names).Error)
	return names
}

func TestTenantRLS_CatalogNoDefinerFunctionHasPublicExecute(t *testing.T) {
	f := catalogClone(t)
	require.Empty(t, publicDefinerFunctions(t, f.owner))
	require.NoError(t, f.owner.Exec(`CREATE FUNCTION public.synthetic_unsafe_definer() RETURNS integer LANGUAGE sql SECURITY DEFINER AS 'SELECT 1'`).Error)
	require.Equal(t, []string{"synthetic_unsafe_definer"}, publicDefinerFunctions(t, f.owner))
	require.NoError(t, f.owner.Exec(`REVOKE EXECUTE ON FUNCTION public.synthetic_unsafe_definer() FROM PUBLIC`).Error)
	require.Empty(t, publicDefinerFunctions(t, f.owner))
}

// Exercise the actual opt-in test entrypoint in a separate binary invocation.
// The caller creates/owns this second, deliberately retained export target;
// ordinary catalog assertions still share only one bootstrap template.
func TestTenantRLS_CatalogCompatExportRetainsTargetAndRefusesRepeat(t *testing.T) {
	f := catalogEmptyTarget(t)
	ownerURL := catalogRoleURL(catalogTemplate.base, f.name, catalogTemplate.owner, catalogTemplate.ownerPassword)
	run := func() ([]byte, error) {
		command := exec.Command("go", "test", "-mod=readonly", "-count=1", "-timeout", "3m", "./internal/database", "-run", "^TestCompatBootstrapExport$", "-v")
		command.Dir = "../.."
		for _, variable := range os.Environ() {
			if !strings.HasPrefix(variable, "COMPAT_BOOTSTRAP_") {
				command.Env = append(command.Env, variable)
			}
		}
		command.Env = append(command.Env, "COMPAT_BOOTSTRAP_DSN="+ownerURL.String(), "COMPAT_BOOTSTRAP_RUNTIME_ROLE="+catalogTemplate.runtime)
		return command.CombinedOutput()
	}
	output, err := run()
	// Never print the explicit descriptor or a child driver error.
	require.NoError(t, err, "synthetic export child failed; output withheld")
	require.Contains(t, string(output), "synthetic compatibility bootstrap exported and future catalog verified")
	require.NoError(t, database.VerifyTenantRLS(f.runtime, catalogTemplate.runtime))
	known, globals := loadCatalogGoldens(t)
	before := captureSyntheticCatalog(t, f, known, globals)
	profile, err := loadProductionProfile()
	require.NoError(t, err)
	require.Equal(t, profile.LiveSHA256, before.SHA256)
	require.Empty(t, before.Unknown)
	output, err = run()
	require.Error(t, err)
	require.Contains(t, string(output), "synthetic compatibility bootstrap refused or failed")
	after := captureSyntheticCatalog(t, f, known, globals)
	require.Equal(t, before, after)
	require.NoError(t, database.VerifyTenantRLS(f.runtime, catalogTemplate.runtime))
}
