package database_test

// This first lane validates a deliberately small synthetic source inventory.
// The real application bootstrap has default ACLs and must remain unsupported.
import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/dbcatalog"
)

const nullableOperationJSON = "{\"column\":\"extra\",\"default\":null,\"format_version\":1,\"id\":\"add_extra\",\"kind\":\"add_nullable_column\",\"nullable\":true,\"relation\":\"nullable_sample\",\"type\":\"pg_catalog.int8\"}\n"

func nullableCatalogOperation(t *testing.T) dbcatalog.NullableColumnOperationV1 {
	t.Helper()
	op, err := dbcatalog.ParseNullableColumnOperationV1([]byte(nullableOperationJSON))
	if err != nil {
		t.Fatal("fixed nullable operation refused")
	}
	return op
}

// These names come from the exact DDL below, never a database discovery query.
func nullableCatalogInventory(after bool) dbcatalog.Catalog {
	objects := []dbcatalog.Object{}
	for _, name := range []string{"organizations", "nullable_sample"} {
		objects = append(objects, dbcatalog.Object{Kind: "relations", Identity: name, Attributes: map[string]string{}}, dbcatalog.Object{Kind: "types", Identity: name, Attributes: map[string]string{}}, dbcatalog.Object{Kind: "types", Identity: "_" + name, Attributes: map[string]string{}})
	}
	for _, pair := range [][2]string{{"organizations", "id"}, {"nullable_sample", "id"}, {"nullable_sample", "label"}} {
		objects = append(objects, dbcatalog.Object{Kind: "columns", Identity: pair[0] + "." + pair[1], Parent: pair[0], Attributes: map[string]string{}})
	}
	objects = append(objects, dbcatalog.Object{Kind: "constraints", Identity: "nullable_sample.nullable_sample_pkey", Parent: "nullable_sample", Attributes: map[string]string{}}, dbcatalog.Object{Kind: "indexes", Identity: "nullable_sample.nullable_sample_pkey", Parent: "nullable_sample", Attributes: map[string]string{}}, dbcatalog.Object{Kind: "indexes", Identity: "nullable_sample.nullable_sample_label_idx", Parent: "nullable_sample", Attributes: map[string]string{}}, dbcatalog.Object{Kind: "sequences", Identity: "nullable_sample_id_seq", Attributes: map[string]string{}})
	if after {
		objects = append(objects, dbcatalog.Object{Kind: "columns", Identity: "nullable_sample.extra", Parent: "nullable_sample", Attributes: map[string]string{}})
	}
	return dbcatalog.Catalog{Version: 0, Objects: objects}
}

func nullableCatalogFixture(t *testing.T) catalogFixture {
	t.Helper()
	f := catalogEmptyTarget(t)
	var major int
	if f.owner.Raw("SELECT current_setting('server_version_num')::integer/10000").Scan(&major).Error != nil || major != 17 {
		t.Fatal("nullable graph qualification requires actual PG17")
	}
	statements := []string{
		`CREATE TABLE public.organizations (id bigint)`,
		`CREATE TABLE public.nullable_sample (id bigint NOT NULL, label text DEFAULT 'baseline'::text, CONSTRAINT nullable_sample_pkey PRIMARY KEY(id))`,
		`CREATE SEQUENCE public.nullable_sample_id_seq OWNED BY public.nullable_sample.id`,
		`ALTER TABLE public.nullable_sample ALTER COLUMN id SET DEFAULT nextval('public.nullable_sample_id_seq'::regclass)`,
		`CREATE INDEX nullable_sample_label_idx ON public.nullable_sample(label)`,
		`INSERT INTO public.nullable_sample(id,label) VALUES (1,'first'),(2,'second')`,
		`GRANT SELECT ON TABLE public.organizations,public.nullable_sample TO ` + catalogTemplate.runtime,
		`GRANT USAGE,SELECT ON SEQUENCE public.nullable_sample_id_seq TO ` + catalogTemplate.runtime,
	}
	for _, sql := range statements {
		if f.owner.Exec(sql).Error != nil {
			t.Fatal("fixed synthetic nullable setup failed")
		}
	}
	var defaults int64
	if f.owner.Raw("SELECT count(*) FROM pg_catalog.pg_default_acl").Scan(&defaults).Error != nil || defaults != 0 {
		t.Fatal("synthetic nullable baseline has unexpected defaults")
	}
	return f
}

func nullableCatalogCapture(t *testing.T, f catalogFixture, after bool) dbcatalog.NullableObservationV1 {
	t.Helper()
	v, err := dbcatalog.CaptureNullableGraphV1(context.Background(), f.runtimeDSN, dbcatalog.Options{Catalog: nullableCatalogInventory(after), Timeout: time.Minute}, nullableCatalogOperation(t))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func nullableCatalogRows(t *testing.T, f catalogFixture) string {
	t.Helper()
	var result string
	if f.runtime.Raw(`SELECT COALESCE(jsonb_agg(jsonb_build_array(id,label) ORDER BY id),'[]'::jsonb)::text FROM public.nullable_sample`).Scan(&result).Error != nil {
		t.Fatal("synthetic old-column read failed")
	}
	return result
}

func nullableCatalogExport(t *testing.T, before, after dbcatalog.NullableObservationV1, explanation dbcatalog.NullableExplanationV1) {
	t.Helper()
	path := os.Getenv("REREPLY_NULLABLE_FIXTURE_OUTPUT")
	if path == "" {
		return
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		t.Fatal("nullable fixture output requires a clean absolute path")
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
		t.Fatal("nullable fixture output directory refused")
	}
	b, err := dbcatalog.CanonicalNullableObservationV1(before)
	if err != nil {
		t.Fatal(err)
	}
	a, err := dbcatalog.CanonicalNullableObservationV1(after)
	if err != nil {
		t.Fatal(err)
	}
	e, err := dbcatalog.CanonicalNullableExplanationV1(explanation)
	if err != nil {
		t.Fatal(err)
	}
	// Each child already has the agreed canonical encoding. RawMessage preserves
	// its sorted keys; json.Marshal sorts this exact five-key outer object too.
	raw, err := json.Marshal(map[string]json.RawMessage{"format_version": []byte("1"), "operation": []byte(nullableOperationJSON), "before": b, "after": a, "explanation": e})
	if err != nil || len(raw)+1 > (8<<20)+(4<<10) {
		t.Fatal("nullable fixture encoding refused")
	}
	raw = append(raw, '\n')
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("nullable fixture create-only output refused")
	}
	n, writeErr := out.Write(raw)
	closeErr := out.Close()
	if writeErr != nil || closeErr != nil || n != len(raw) {
		t.Fatal("nullable fixture output incomplete")
	}
	t.Logf("complete synthetic nullable fixture exported: %d bytes", len(raw))
}

func TestTenantRLS_CatalogSemanticsNullablePositive(t *testing.T) {
	f := nullableCatalogFixture(t)
	op := nullableCatalogOperation(t)
	before := nullableCatalogCapture(t, f, false)
	rows := nullableCatalogRows(t, f)
	statement, err := dbcatalog.RenderNullableColumnV1(op)
	if err != nil {
		t.Fatal(err)
	}
	if f.runtime.Exec(string(statement)).Error == nil {
		t.Fatal("runtime role unexpectedly executed DDL")
	}
	if f.owner.Exec(string(statement)).Error != nil {
		t.Fatal("synthetic owner nullable statement failed")
	}
	after := nullableCatalogCapture(t, f, true)
	if got := nullableCatalogRows(t, f); got != rows {
		t.Fatal("old synthetic row values changed")
	}
	var total, nulls int64
	if f.runtime.Raw(`SELECT count(*),count(*) FILTER (WHERE extra IS NULL) FROM public.nullable_sample`).Row().Scan(&total, &nulls) != nil || total != 2 || nulls != total {
		t.Fatal("new values were not all NULL")
	}
	explanation, err := dbcatalog.ExplainNullableColumnV1(op, before, after)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []dbcatalog.NullableObservationV1{before, after} {
		b, err := dbcatalog.CanonicalNullableObservationV1(value)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := dbcatalog.ParseNullableObservationV1(b)
		if err != nil {
			t.Fatal(err)
		}
		again, err := dbcatalog.CanonicalNullableObservationV1(parsed)
		if err != nil || !bytes.Equal(b, again) {
			t.Fatal("actual observation roundtrip changed")
		}
	}
	nullableCatalogExport(t, before, after, explanation)
}

func TestTenantRLS_CatalogSemanticsApplicationDefaultsRemainUnsupported(t *testing.T) {
	f := catalogClone(t)
	known, globals := loadCatalogGoldens(t)
	var defaults int64
	if f.owner.Raw("SELECT count(*) FROM pg_catalog.pg_default_acl").Scan(&defaults).Error != nil || defaults == 0 {
		t.Fatal("application baseline default ACL premise changed")
	}
	raw := bytes.Replace([]byte(nullableOperationJSON), []byte(`"relation":"nullable_sample"`), []byte(`"relation":"organizations"`), 1)
	op, err := dbcatalog.ParseNullableColumnOperationV1(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbcatalog.CaptureNullableGraphV1(context.Background(), f.runtimeDSN, dbcatalog.Options{Catalog: known, GlobalTables: globals, Timeout: time.Minute}, op); err == nil {
		t.Fatal("application default ACL baseline falsely qualified")
	}
}

func TestTenantRLS_CatalogSemanticsUnsupportedLayouts(t *testing.T) {
	for name, sql := range map[string]string{
		"dropped slot":       `ALTER TABLE public.nullable_sample DROP COLUMN label CASCADE`,
		"child":              `CREATE TABLE public.nullable_child () INHERITS (public.nullable_sample)`,
		"foreign child":      `CREATE SCHEMA nullable_other; CREATE TABLE nullable_other.child () INHERITS (public.nullable_sample)`,
		"foreign principal":  `GRANT SELECT ON public.nullable_sample TO CURRENT_USER WITH GRANT OPTION; GRANT SELECT ON public.nullable_sample TO pg_monitor`,
		"default privileges": `ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO PUBLIC`,
	} {
		t.Run(name, func(t *testing.T) {
			f := nullableCatalogFixture(t)
			if f.owner.Exec(sql).Error != nil {
				t.Fatal("synthetic negative layout setup failed")
			}
			if _, err := dbcatalog.CaptureNullableGraphV1(context.Background(), f.runtimeDSN, dbcatalog.Options{Catalog: nullableCatalogInventory(false), Timeout: time.Minute}, nullableCatalogOperation(t)); err == nil {
				t.Fatal("unsupported layout captured as valid")
			}
		})
	}
}

func TestTenantRLS_CatalogSemanticsActualExtraChangesRefuse(t *testing.T) {
	for name, sql := range map[string]string{
		"non-null default": `ALTER TABLE public.nullable_sample ALTER COLUMN extra SET DEFAULT 1::bigint`,
		"not null":         `UPDATE public.nullable_sample SET extra=9; ALTER TABLE public.nullable_sample ALTER COLUMN extra SET NOT NULL`,
		"old default":      `ALTER TABLE public.nullable_sample ALTER COLUMN label SET DEFAULT 'different'::text`,
		"old ACL":          `REVOKE SELECT ON public.nullable_sample FROM ` + catalogTemplate.runtime,
		"schema ACL":       `REVOKE USAGE ON SCHEMA public FROM PUBLIC`,
		"database ACL":     `REVOKE TEMPORARY ON DATABASE `,
		"extra index":      `CREATE INDEX nullable_extra_idx ON public.nullable_sample(extra)`,
	} {
		t.Run(name, func(t *testing.T) {
			f := nullableCatalogFixture(t)
			before := nullableCatalogCapture(t, f, false)
			op := nullableCatalogOperation(t)
			statement, err := dbcatalog.RenderNullableColumnV1(op)
			if err != nil {
				t.Fatal(err)
			}
			if f.owner.Exec(string(statement)).Error != nil {
				t.Fatal("synthetic nullable operation failed")
			}
			if name == "old ACL" {
				sql = `REVOKE SELECT ON public.nullable_sample FROM ` + catalogTemplate.runtime
			}
			if name == "database ACL" {
				sql += f.name + ` FROM PUBLIC`
			}
			if f.owner.Exec(sql).Error != nil {
				t.Fatal("synthetic negative change failed")
			}
			if name == "non-null default" {
				var hasDefault bool
				var defaults int64
				if f.owner.Raw(`SELECT a.atthasdef,(SELECT count(*) FROM pg_catalog.pg_attrdef d WHERE d.adrelid=a.attrelid AND d.adnum=a.attnum) FROM pg_catalog.pg_attribute a WHERE a.attrelid='public.nullable_sample'::regclass AND a.attname='extra' AND a.attnum>0 AND NOT a.attisdropped`).Row().Scan(&hasDefault, &defaults) != nil || !hasDefault || defaults != 1 {
					t.Fatal("non-null default premise was not stored in pg_attrdef")
				}
			}
			after, err := dbcatalog.CaptureNullableGraphV1(context.Background(), f.runtimeDSN, dbcatalog.Options{Catalog: nullableCatalogInventory(true), Timeout: time.Minute}, op)
			if err == nil {
				if _, err = dbcatalog.ExplainNullableColumnV1(op, before, after); err == nil {
					t.Fatal("unexplained actual change accepted")
				}
			}
		})
	}
}

// PostgreSQL17 discards a bare NULL default for an ordinary int8 column.
// Endpoint evidence intentionally cannot distinguish that catalog no-op from
// not issuing the SQL; this does not authorize arbitrary statement execution.
func TestTenantRLS_CatalogSemanticsNullDefaultIsCatalogNoop(t *testing.T) {
	f := nullableCatalogFixture(t)
	op := nullableCatalogOperation(t)
	statement, err := dbcatalog.RenderNullableColumnV1(op)
	if err != nil || f.owner.Exec(string(statement)).Error != nil {
		t.Fatal("synthetic nullable operation failed")
	}
	before, err := dbcatalog.CanonicalNullableObservationV1(nullableCatalogCapture(t, f, true))
	if err != nil {
		t.Fatal(err)
	}
	if f.owner.Exec(`ALTER TABLE public.nullable_sample ALTER COLUMN extra SET DEFAULT NULL::bigint`).Error != nil {
		t.Fatal("synthetic NULL default statement failed")
	}
	var hasDefault bool
	var defaults int64
	if f.owner.Raw(`SELECT a.atthasdef,(SELECT count(*) FROM pg_catalog.pg_attrdef d WHERE d.adrelid=a.attrelid AND d.adnum=a.attnum) FROM pg_catalog.pg_attribute a WHERE a.attrelid='public.nullable_sample'::regclass AND a.attname='extra' AND a.attnum>0 AND NOT a.attisdropped`).Row().Scan(&hasDefault, &defaults) != nil || hasDefault || defaults != 0 {
		t.Fatal("NULL default was not normalized to catalog absence")
	}
	after, err := dbcatalog.CanonicalNullableObservationV1(nullableCatalogCapture(t, f, true))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("NULL default changed the complete canonical observation")
	}
}

func TestTenantRLS_CatalogSemanticsCancelledAndIncompleteRefuse(t *testing.T) {
	f := nullableCatalogFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dbcatalog.CaptureNullableGraphV1(ctx, f.runtimeDSN, dbcatalog.Options{Catalog: nullableCatalogInventory(false), Timeout: time.Minute}, nullableCatalogOperation(t)); err == nil {
		t.Fatal("cancelled capture emitted an observation")
	}
	incomplete := nullableCatalogInventory(false)
	incomplete.Objects = incomplete.Objects[:len(incomplete.Objects)-1]
	if _, err := dbcatalog.CaptureNullableGraphV1(context.Background(), f.runtimeDSN, dbcatalog.Options{Catalog: incomplete, Timeout: time.Minute}, nullableCatalogOperation(t)); err == nil {
		t.Fatal("incomplete inventory emitted an observation")
	}
	missing := nullableCatalogInventory(true)
	if _, err := dbcatalog.CaptureNullableGraphV1(context.Background(), f.runtimeDSN, dbcatalog.Options{Catalog: missing, Timeout: time.Minute}, nullableCatalogOperation(t)); err == nil {
		t.Fatal("missing expected column emitted an observation")
	}
}
