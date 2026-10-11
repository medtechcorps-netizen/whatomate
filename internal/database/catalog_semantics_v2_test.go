package database_test

// Actual application-shape qualification on owned clones. No baseline defaults
// are revoked for a positive, and the equality inventory is never DB-learned.
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

const nullableV2OperationJSON = "{\"column\":\"extra\",\"default\":null,\"format_version\":1,\"id\":\"add_extra\",\"kind\":\"add_nullable_column\",\"nullable\":true,\"relation\":\"organizations\",\"type\":\"pg_catalog.int8\"}\n"

func nullableV2Operation(t *testing.T) dbcatalog.NullableColumnOperationV1 {
	t.Helper()
	op, err := dbcatalog.ParseNullableColumnOperationV1([]byte(nullableV2OperationJSON))
	if err != nil {
		t.Fatal(err)
	}
	return op
}
func nullableV2Options(t *testing.T, after bool) dbcatalog.Options {
	t.Helper()
	known, globals := loadCatalogGoldens(t)
	if after {
		known.Objects = append(known.Objects, dbcatalog.Object{Kind: "columns", Identity: "organizations.extra", Parent: "organizations", Attributes: map[string]string{}})
	}
	return dbcatalog.Options{Catalog: known, GlobalTables: globals, Timeout: time.Minute}
}
func nullableV2Application(t *testing.T) catalogFixture {
	t.Helper()
	f := catalogClone(t)
	var major int
	var defaults int64
	if f.owner.Raw("SELECT current_setting('server_version_num')::integer/10000").Scan(&major).Error != nil || major != 17 {
		t.Fatal("actual PG17 required")
	}
	if f.owner.Raw("SELECT count(*) FROM pg_catalog.pg_default_acl").Scan(&defaults).Error != nil || defaults != 2 {
		t.Fatal("actual unchanged application defaults premise differs")
	}
	return f
}
func nullableV2Capture(t *testing.T, f catalogFixture, after bool) dbcatalog.NullableObservationV2 {
	t.Helper()
	v, err := dbcatalog.CaptureNullableGraphV2(context.Background(), f.runtimeDSN, nullableV2Options(t, after), nullableV2Operation(t))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func nullableV2Apply(t *testing.T, f catalogFixture) {
	t.Helper()
	sql, err := dbcatalog.RenderNullableColumnV1(nullableV2Operation(t))
	if err != nil {
		t.Fatal(err)
	}
	if f.runtime.Exec(string(sql)).Error == nil {
		t.Fatal("runtime unexpectedly executed DDL")
	}
	if f.owner.Exec(string(sql)).Error != nil {
		t.Fatal("fixed owner nullable statement failed")
	}
}
func nullableV2OldRows(t *testing.T, f catalogFixture) string {
	t.Helper()
	var s string
	if f.owner.Raw(`SELECT COALESCE(jsonb_agg(to_jsonb(o)-'extra' ORDER BY o.id),'[]'::jsonb)::text FROM public.organizations o`).Scan(&s).Error != nil || s == "[]" {
		t.Fatal("application old-row premise missing")
	}
	return s
}
func nullableV2Export(t *testing.T, b, a dbcatalog.NullableObservationV2, e dbcatalog.NullableExplanationV2) {
	t.Helper()
	path := os.Getenv("REREPLY_NULLABLE_V2_FIXTURE_OUTPUT")
	if path == "" {
		return
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		t.Fatal("v2 fixture output must be clean absolute path")
	}
	st, err := os.Lstat(filepath.Dir(path))
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("v2 fixture parent refused")
	}
	before, err := dbcatalog.CanonicalNullableObservationV2(b)
	if err != nil {
		t.Fatal(err)
	}
	after, err := dbcatalog.CanonicalNullableObservationV2(a)
	if err != nil {
		t.Fatal(err)
	}
	explain, err := dbcatalog.CanonicalNullableExplanationV2(e)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]json.RawMessage{"format_version": []byte("2"), "operation": []byte(nullableV2OperationJSON), "before": before, "after": after, "explanation": explain})
	if err != nil || len(raw)+1 > (8<<20)+(4<<10) {
		t.Fatal("v2 fixture bounds")
	}
	raw = append(raw, '\n')
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("v2 fixture create-only output refused")
	}
	n, werr := out.Write(raw)
	cerr := out.Close()
	if werr != nil || cerr != nil || n != len(raw) {
		t.Fatal("v2 fixture incomplete")
	}
	t.Logf("complete actual application fixture exported: %d bytes", len(raw))
}
func TestTenantRLS_CatalogSemanticsV2ApplicationPositive(t *testing.T) {
	f := nullableV2Application(t)
	before := nullableV2Capture(t, f, false)
	old := nullableV2OldRows(t, f)
	// V1 retains its explicit refusal of this actual application baseline.
	if _, err := dbcatalog.CaptureNullableGraphV1(context.Background(), f.runtimeDSN, nullableV2Options(t, false), nullableV2Operation(t)); err == nil {
		t.Fatal("v1 application-default refusal weakened")
	}
	nullableV2Apply(t, f)
	after := nullableV2Capture(t, f, true)
	if nullableV2OldRows(t, f) != old {
		t.Fatal("old application row projection changed")
	}
	var count, nulls int64
	if f.owner.Raw(`SELECT count(*),count(*) FILTER (WHERE extra IS NULL) FROM public.organizations`).Row().Scan(&count, &nulls) != nil || count < 1 || count != nulls {
		t.Fatal("new application column not NULL in every old row")
	}
	e, err := dbcatalog.ExplainNullableColumnV2(nullableV2Operation(t), before, after)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []dbcatalog.NullableObservationV2{before, after} {
		raw, err := dbcatalog.CanonicalNullableObservationV2(v)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := dbcatalog.ParseNullableObservationV2(raw)
		if err != nil {
			t.Fatal(err)
		}
		again, err := dbcatalog.CanonicalNullableObservationV2(parsed)
		if err != nil || !bytes.Equal(raw, again) {
			t.Fatal("actual v2 roundtrip drift")
		}
	}
	nullableV2Export(t, before, after, e)
}
func TestTenantRLS_CatalogSemanticsV2DefaultChangesRefuse(t *testing.T) {
	changes := map[string]func(catalogFixture) string{
		"table swap": func(f catalogFixture) string {
			return `ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE DELETE ON TABLES FROM ` + catalogTemplate.runtime + `; ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT TRUNCATE ON TABLES TO ` + catalogTemplate.runtime
		},
		"sequence swap": func(f catalogFixture) string {
			return `ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE SELECT ON SEQUENCES FROM ` + catalogTemplate.runtime + `; ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT UPDATE ON SEQUENCES TO ` + catalogTemplate.runtime
		},
		"grant option": func(f catalogFixture) string {
			return `ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE ON SEQUENCES TO ` + catalogTemplate.runtime + ` WITH GRANT OPTION`
		},
		"PUBLIC": func(f catalogFixture) string {
			return `ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO PUBLIC`
		},
		"global override": func(f catalogFixture) string {
			return `ALTER DEFAULT PRIVILEGES GRANT SELECT ON TABLES TO ` + catalogTemplate.runtime
		},
		"function kind": func(f catalogFixture) string {
			return `ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT EXECUTE ON FUNCTIONS TO ` + catalogTemplate.runtime
		},
		"row removal": func(f catalogFixture) string {
			return `ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON SEQUENCES FROM ` + catalogTemplate.runtime
		},
		"unknown principal": func(f catalogFixture) string {
			return `ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO pg_monitor`
		},
		"unknown namespace": func(f catalogFixture) string {
			return `CREATE SCHEMA nullable_unexpected; ALTER DEFAULT PRIVILEGES IN SCHEMA nullable_unexpected GRANT SELECT ON TABLES TO ` + catalogTemplate.runtime
		},
	}
	for name, sql := range changes {
		t.Run(name, func(t *testing.T) {
			f := nullableV2Application(t)
			before := nullableV2Capture(t, f, false)
			nullableV2Apply(t, f)
			if f.owner.Exec(sql(f)).Error != nil {
				t.Fatal("actual default mutation failed")
			}
			after, err := dbcatalog.CaptureNullableGraphV2(context.Background(), f.runtimeDSN, nullableV2Options(t, true), nullableV2Operation(t))
			if err == nil {
				if _, err := dbcatalog.ExplainNullableColumnV2(nullableV2Operation(t), before, after); err == nil {
					t.Fatal("changed actual defaults explained")
				}
			}
		})
	}
}
func TestTenantRLS_CatalogSemanticsV2EqualUnexpectedDefaultsRefuse(t *testing.T) {
	f := nullableV2Application(t)
	if f.owner.Exec(`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE ON SEQUENCES TO `+catalogTemplate.runtime+` WITH GRANT OPTION`).Error != nil {
		t.Fatal("unexpected defaults setup failed")
	}
	before := nullableV2Capture(t, f, false)
	nullableV2Apply(t, f)
	after := nullableV2Capture(t, f, true)
	if _, err := dbcatalog.ExplainNullableColumnV2(nullableV2Operation(t), before, after); err == nil {
		t.Fatal("unchanged unexpected actual ACL contract accepted")
	}
}
func TestTenantRLS_CatalogSemanticsV2OtherChangesRefuse(t *testing.T) {
	for name, sql := range map[string]string{
		"non-null default": `ALTER TABLE public.organizations ALTER COLUMN extra SET DEFAULT 1::bigint`,
		"not null":         `UPDATE public.organizations SET extra=1; ALTER TABLE public.organizations ALTER COLUMN extra SET NOT NULL`,
		"wrong type":       `ALTER TABLE public.organizations ALTER COLUMN extra TYPE integer`,
		"extra index":      `CREATE INDEX nullable_v2_unexpected ON public.organizations(extra)`,
		"old ACL":          `GRANT TRUNCATE ON public.organizations TO PUBLIC`,
		"schema ACL":       `GRANT CREATE ON SCHEMA public TO PUBLIC`,
		"dropped slot":     `ALTER TABLE public.organizations ADD COLUMN nullable_removed bigint; ALTER TABLE public.organizations DROP COLUMN nullable_removed`,
	} {
		t.Run(name, func(t *testing.T) {
			f := nullableV2Application(t)
			before := nullableV2Capture(t, f, false)
			nullableV2Apply(t, f)
			if f.owner.Exec(sql).Error != nil {
				t.Fatal("actual extra change failed")
			}
			if name == "non-null default" {
				var n int64
				if f.owner.Raw(`SELECT count(*) FROM pg_catalog.pg_attrdef d JOIN pg_catalog.pg_attribute a ON a.attrelid=d.adrelid AND a.attnum=d.adnum WHERE a.attrelid='public.organizations'::regclass AND a.attname='extra'`).Scan(&n).Error != nil || n != 1 {
					t.Fatal("default presence premise failed")
				}
			}
			after, err := dbcatalog.CaptureNullableGraphV2(context.Background(), f.runtimeDSN, nullableV2Options(t, true), nullableV2Operation(t))
			if err == nil {
				if _, err := dbcatalog.ExplainNullableColumnV2(nullableV2Operation(t), before, after); err == nil {
					t.Fatal("extra catalog delta accepted")
				}
			}
		})
	}
}
func TestTenantRLS_CatalogSemanticsV2CancelledAndIncomplete(t *testing.T) {
	f := nullableV2Application(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dbcatalog.CaptureNullableGraphV2(ctx, f.runtimeDSN, nullableV2Options(t, false), nullableV2Operation(t)); err == nil {
		t.Fatal("cancelled observation published")
	}
	opt := nullableV2Options(t, false)
	opt.Catalog.Objects = opt.Catalog.Objects[1:]
	if _, err := dbcatalog.CaptureNullableGraphV2(context.Background(), f.runtimeDSN, opt, nullableV2Operation(t)); err == nil {
		t.Fatal("incomplete source inventory published")
	}
	if _, err := dbcatalog.CaptureNullableGraphV2(context.Background(), f.runtimeDSN, nullableV2Options(t, true), nullableV2Operation(t)); err == nil {
		t.Fatal("missing column observation published")
	}
}
func TestTenantRLS_CatalogSemanticsV2ConcurrentDefaultsRemainConsistent(t *testing.T) {
	f := nullableV2Application(t)
	done := make(chan error, 1)
	// The writer changes only this clone. Each capture must return one complete
	// snapshot, including when the sequence-default row is temporarily absent.
	go func() {
		for i := 0; i < 4; i++ {
			if err := f.owner.Exec(`ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON SEQUENCES FROM ` + catalogTemplate.runtime).Error; err != nil {
				done <- err
				return
			}
			time.Sleep(15 * time.Millisecond)
			if err := f.owner.Exec(`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT,USAGE ON SEQUENCES TO ` + catalogTemplate.runtime).Error; err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	var captureErr error
	for i := 0; i < 3; i++ {
		v, err := dbcatalog.CaptureNullableGraphV2(context.Background(), f.runtimeDSN, nullableV2Options(t, false), nullableV2Operation(t))
		if err != nil {
			captureErr = err
			break
		}
		if _, err = dbcatalog.CanonicalNullableObservationV2(v); err != nil {
			captureErr = err
			break
		}
	}
	if err := <-done; err != nil {
		t.Fatal("isolated concurrent defaults write failed")
	}
	if captureErr != nil {
		t.Fatal("capture did not retain one complete snapshot", captureErr)
	}
}
