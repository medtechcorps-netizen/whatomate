package dbcatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testObject() Object {
	return Object{Kind: "relations", Identity: "organizations", Attributes: map[string]string{"relkind": "r", "owner": "MIGRATION_OWNER", "rls": "true", "force_rls": "true", "acl": "", "security_invoker": "false", "view_sha256": digest(nil), "partition_key_sha256": digest(nil), "partition_bound_sha256": digest(nil), "parents_sha256": digest([]byte("[]"))}}
}
func testCatalog() Catalog { return Catalog{Version: 0, Objects: []Object{testObject()}} }
func testSnapshot(t *testing.T) Snapshot {
	t.Helper()
	s, e := project([]Object{testObject()}, testCatalog(), Ownership{}, map[string]int64{})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func requireError(t *testing.T, e error, code string) {
	t.Helper()
	if e == nil || e.Error() != "catalog-snapshot:"+code {
		t.Fatalf("expected safe code %s; got %v", code, e)
	}
}

func TestUnknownExtrasDoNotChangeKnownHash(t *testing.T) {
	base := testSnapshot(t)
	all := []Object{testObject(), {Kind: "relations", Identity: "private_customer_123", Attributes: map[string]string{}}, {Kind: "columns", Identity: "organizations.secret_column", Parent: "organizations", Attributes: map[string]string{}}, {Kind: "columns", Identity: "private_customer_123.customer_email", Parent: "private_customer_123", Attributes: map[string]string{}}}
	counts := map[string]int64{"non_public_schemas": 9, "other_roles": 41}
	s, err := project(all, testCatalog(), Ownership{}, counts)
	if err != nil {
		t.Fatal(err)
	}
	if s.SHA256 != base.SHA256 {
		t.Fatal("unknown objects changed equality hash")
	}
	if len(s.Unknown) != 3 {
		t.Fatal(s.Unknown)
	}
	for _, mode := range []string{"json", "compare", "sha256"} {
		var out bytes.Buffer
		if err = WritePublic(&out, s, testCatalog(), mode); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"private_customer_123", "secret_column", "customer_email"} {
			if strings.Contains(out.String(), secret) {
				t.Fatal("unknown identity leaked")
			}
		}
	}
	if len(UnknownIdentities(s)) != 3 {
		t.Fatal("explicit owner listing absent")
	}
}

func TestMissingAndKnownAttributeChangesAffectHash(t *testing.T) {
	base := testSnapshot(t)
	missing, err := project(nil, testCatalog(), Ownership{}, map[string]int64{})
	if err != nil {
		t.Fatal(err)
	}
	if !missing.Objects[0].Missing || missing.SHA256 == base.SHA256 {
		t.Fatal("missing object not bound")
	}
	changed := testObject()
	changed.Attributes["force_rls"] = "false"
	s, err := project([]Object{changed}, testCatalog(), Ownership{}, map[string]int64{})
	if err != nil {
		t.Fatal(err)
	}
	if s.SHA256 == base.SHA256 {
		t.Fatal("material attribute not bound")
	}
	diffs, err := Compare(s, testCatalog())
	if err != nil || len(diffs) != 1 || diffs[0].Status != "changed" {
		t.Fatal(diffs, err)
	}
	diffs, err = Compare(missing, testCatalog())
	if err != nil || len(diffs) != 1 || diffs[0].Status != "missing" {
		t.Fatal(diffs, err)
	}
}

func TestRoleSymbolizationAndFingerprintLiteral(t *testing.T) {
	roles := symbols{"123", "456"}
	for oid, want := range map[string]string{"0": "PUBLIC", "123": "MIGRATION_OWNER", "456": "RUNTIME", "789": "OTHER"} {
		if roles.role(oid) != want {
			t.Fatal(oid)
		}
	}
	fp := strings.Repeat("a", 64)
	source := "\n SELECT '" + fp + "'::text\n"
	definition := "CREATE FUNCTION x AS $$" + source + "$$"
	a, b := normalizeFunction("rereply_tenant_policy_fingerprint", source, definition)
	if strings.Contains(a, fp) || strings.Contains(b, fp) || !strings.Contains(a, "<FINGERPRINT>") {
		t.Fatal("fingerprint normalization")
	}
	for _, name := range []string{"unknown_fn", "rereply_tenant_policy_fingerprint_suffix"} {
		a, b = normalizeFunction(name, source, definition)
		if a != source || b != definition {
			t.Fatal("unrelated source altered")
		}
	}
	bad := source + "; SELECT 1"
	a, _ = normalizeFunction("rereply_tenant_policy_fingerprint", bad, definition)
	if a != bad {
		t.Fatal("modified fingerprint body laundered")
	}
}

func TestAdditiveFingerprintVersionedDigestOnly(t *testing.T) {
	const name = "rereply_tenant_policy_additive_fingerprint_v1"
	source := "\n  SELECT 'v1:" + strings.Repeat("a", 64) + "'::text\n"
	definition := "CREATE FUNCTION " + name + "() AS $$" + source + "$$"
	otherSource := strings.ReplaceAll(source, strings.Repeat("a", 64), strings.Repeat("b", 64))
	otherDefinition := strings.ReplaceAll(definition, strings.Repeat("a", 64), strings.Repeat("b", 64))
	a, b := normalizeFunction(name, source, definition)
	c, d := normalizeFunction(name, otherSource, otherDefinition)
	if a != c || b != d || !strings.Contains(a, "'v1:<FINGERPRINT>'::text") {
		t.Fatal("versioned additive digest normalization")
	}
	for _, invalid := range []struct{ name, source string }{
		{"rereply_tenant_policy_fingerprint", source},
		{"rereply_tenant_policy_additive_fingerprint_v2", source},
		{name + "_suffix", source},
		{name, strings.ReplaceAll(source, "v1:", "v2:")},
		{name, strings.ReplaceAll(source, "v1:", "")},
		{name, source + "; SELECT 1"},
	} {
		def := "CREATE FUNCTION x AS $$" + invalid.source + "$$"
		x, y := normalizeFunction(invalid.name, invalid.source, def)
		if x != invalid.source || y != def {
			t.Fatal("foreign name, version or body normalized")
		}
	}
}

func TestACLAndPolicyRolesNeverExposeOIDs(t *testing.T) {
	attrs := map[string]json.RawMessage{}
	payload := `{"command":"*","permissive":true,"roles":[123,456,789],"using":"organization_id = 123","check":"true"}`
	if json.Unmarshal([]byte(payload), &attrs) != nil {
		t.Fatal("fixture")
	}
	counts := map[string]int64{}
	out, err := normalize("policies", "policy", attrs, symbols{"123", "456"}, counts)
	if err != nil {
		t.Fatal(err)
	}
	if out["roles"] != "MIGRATION_OWNER,RUNTIME" || counts["other_policy_roles"] != 1 {
		t.Fatal(out, counts)
	}
	encoded, _ := json.Marshal(out)
	if strings.Contains(string(encoded), "789") || strings.Contains(string(encoded), "organization_id = 123") {
		t.Fatal("raw metadata leaked")
	}
	var one, two map[string]json.RawMessage
	json.Unmarshal([]byte(`{"relkind":"r","rls":true,"force_rls":true,"owner":"123","acl":[{"grantee":"456","privilege":"SELECT","grantable":false},{"grantee":"789","privilege":"SELECT","grantable":false}],"security_invoker":false,"view":"","partition_key":"","partition_bound":"","parents":"[]"}`), &one)
	json.Unmarshal([]byte(`{"relkind":"r","rls":true,"force_rls":true,"owner":"901","acl":[{"grantee":"902","privilege":"SELECT","grantable":false},{"grantee":"903","privilege":"SELECT","grantable":false}],"security_invoker":false,"view":"","partition_key":"","partition_bound":"","parents":"[]"}`), &two)
	a, e := normalize("relations", "organizations", one, symbols{"123", "456"}, map[string]int64{})
	if e != nil {
		t.Fatal(e)
	}
	b, e := normalize("relations", "organizations", two, symbols{"901", "902"}, map[string]int64{})
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("role names/OIDs affect symbolic attributes")
	}
}

func TestCanonicalOrderAndOverlayIdentityBinding(t *testing.T) {
	a := testObject()
	b := testObject()
	b.Identity = "permissions"
	one, e := CanonicalCatalog(Catalog{Version: 0, Objects: []Object{a, b}})
	if e != nil {
		t.Fatal(e)
	}
	two, e := CanonicalCatalog(Catalog{Version: 0, Objects: []Object{b, a}})
	if e != nil || !bytes.Equal(one, two) {
		t.Fatal("not canonical")
	}
	_, e = mergedCatalog(testCatalog(), &Catalog{Version: 0, Objects: []Object{a}})
	requireError(t, e, "overlay-overlap")
	known, e := mergedCatalog(testCatalog(), &Catalog{Version: 0, Objects: []Object{b}})
	if e != nil || len(known.Objects) != 2 {
		t.Fatal(e)
	}
	s, e := project([]Object{a}, known, Ownership{}, map[string]int64{})
	if e != nil || len(s.Objects) != 2 || !s.Objects[1].Missing {
		t.Fatal("overlay missing not bound", e)
	}
}

func TestStrictCatalogLoadingRejectsMalformedAndPrivateValues(t *testing.T) {
	valid, _ := CanonicalCatalog(testCatalog())
	if _, e := LoadCatalog(valid); e != nil {
		t.Fatal(e)
	}
	for _, data := range []string{`{"version":0,"version":0,"objects":[]}`, `{"version":1,"objects":[]}`, `{"version":0,"objects":[],"private":"token"}`, `{"version":0,"objects":[]} {}`, `{"version":NaN,"objects":[]}`, `{"version":0,"objects":[{"kind":"relations","identity":"org","attributes":{"owner":"alice@example.com"}}]}`, `{"version":0,"objects":[{"kind":"relations","identity":"de305d54-75b4-431b-adb2-eb6b9e546014","attributes":{}}]}`} {
		if _, e := LoadCatalog([]byte(data)); e == nil {
			t.Fatalf("accepted malformed catalog: %s", data)
		}
	}
	for _, data := range []string{`[{"name":"permissions","reason":"global permission definitions","keys":["resource","action"]}]`, `[{"name":"global_flags","reason":"global relation without tenant state"}]`} {
		if _, e := LoadGlobalTables([]byte(data)); e != nil {
			t.Fatal(e)
		}
	}
	for _, data := range []string{`[{"name":"users; DROP TABLE users","reason":"malicious key"}]`, `[{"name":"users","reason":"short"}]`, `[{"name":"users","reason":"some justification","keys":["email","email"]}]`} {
		if _, e := LoadGlobalTables([]byte(data)); e == nil {
			t.Fatal("bad global allowlist")
		}
	}
}

func TestStartupConfigForcesReadOnlyAndBounds(t *testing.T) {
	cfg, e := connectionConfig("postgres://runtime:private-password@localhost/db?default_transaction_read_only=off&search_path=public&statement_timeout=0&lock_timeout=0", 5*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	want := map[string]string{"default_transaction_read_only": "on", "search_path": "pg_catalog", "statement_timeout": "5000", "lock_timeout": "2000", "application_name": "rereply-catalog-snapshot"}
	for key, v := range want {
		if cfg.RuntimeParams[key] != v {
			t.Fatal(key)
		}
	}
	if cfg.ConnectTimeout != 5*time.Second {
		t.Fatal("connect timeout")
	}
	_, e = connectionConfig("postgres://x:do-not-print@[bad", time.Second)
	requireError(t, e, "connect")
	_, e = Capture(context.Background(), "do-not-print", Options{Catalog: testCatalog(), Timeout: 3 * time.Minute})
	requireError(t, e, "timeout")
	_, e = Capture(context.Background(), "do-not-print", Options{})
	requireError(t, e, "empty-catalog")
}

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, errors.New("secret socket endpoint") }
func TestPrinterRejectsUnvalidatedSnapshotBeforeWriting(t *testing.T) {
	s := testSnapshot(t)
	requireError(t, WritePublic(failedWriter{}, s, testCatalog(), "json"), "output")
	for _, edit := range []func(*Snapshot){func(s *Snapshot) { s.SHA256 = strings.Repeat("a", 64) }, func(s *Snapshot) { s.CountOnly = map[string]int64{"secret_role": 1} }, func(s *Snapshot) { s.Unknown = []UnknownCount{{Kind: "relations", Parent: "secret_tenant", Count: 1}} }, func(s *Snapshot) { s.Seeds = []Seed{{Table: "users", Count: 1, KeysSHA256: strings.Repeat("a", 64)}} }, func(s *Snapshot) { s.Objects[0].Attributes["email"] = "owner@example.com" }} {
		s = testSnapshot(t)
		edit(&s)
		var out bytes.Buffer
		if WritePublic(&out, s, testCatalog(), "json") == nil || out.Len() != 0 {
			t.Fatal("printer wrote unsafe snapshot")
		}
	}
}

func TestPackageDoesNotImportMigrationsAndQueriesAreCatalogOnly(t *testing.T) {
	files, e := filepath.Glob("*.go")
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, e := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if e != nil {
			t.Fatal(e)
		}
		for _, imp := range f.Imports {
			if strings.Contains(imp.Path.Value, "internal/database") {
				t.Fatal("migration import")
			}
		}
	}
	for _, q := range queries {
		if !strings.HasPrefix(q.sql, "SELECT ") || !strings.Contains(q.sql, "pg_catalog.") || !strings.Contains(q.sql, "n.nspname='public'") {
			t.Fatal("query contract", q.kind)
		}
	}
	data, e := os.ReadFile("capture.go")
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(data, []byte("IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly")) {
		t.Fatal("transaction contract")
	}
}

func FuzzPublicPrinterRejectsUntrustedAttributes(f *testing.F) {
	for _, seed := range []string{"person@example.com", "de305d54-75b4-431b-adb2-eb6b9e546014", "{\"customer\":\"private\"}", "private_access_token_123456", "MIGRATION_OWNER", "RUNTIME", "MIGRATION_OWNER>PUBLIC:EXECUTE:false", "MIGRATION_OWNER>PUBLIC:CUSTOMER_SECRET:false"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 4096 {
			t.Skip()
		}
		for _, field := range []string{"owner", "acl", "unrecognized"} {
			s := testSnapshot(t)
			s.Objects[0].Attributes[field] = raw
			// Recompute the ordinary canonical bytes directly. The printer itself
			// must validate field values, rather than reject merely a stale hash.
			canonical, _ := json.Marshal(Catalog{Version: 0, Objects: s.Objects})
			s.SHA256 = digest(append(canonical, '\n'))
			var out bytes.Buffer
			err := WritePublic(&out, s, testCatalog(), "json")
			typ, recognized := attributes["relations"][field]
			if recognized && safeAttribute(typ, raw) {
				if err != nil || out.Len() == 0 {
					t.Fatal("valid symbolic value refused")
				}
			} else if err == nil || out.Len() != 0 {
				t.Fatal("untrusted typed value reached printer")
			}
		}
	})
}

func FuzzUnknownNamesRemainCountOnly(f *testing.F) {
	f.Add("owner@example.com")
	f.Add("de305d54-75b4-431b-adb2-eb6b9e546014")
	f.Fuzz(func(t *testing.T, name string) {
		if len(name) > 4096 || name == "organizations" {
			t.Skip()
		}
		s, e := project([]Object{testObject(), {Kind: "relations", Identity: name, Attributes: map[string]string{}}}, testCatalog(), Ownership{}, map[string]int64{})
		if e != nil {
			t.Fatal(e)
		}
		var out bytes.Buffer
		if e = WritePublic(&out, s, testCatalog(), "json"); e != nil {
			t.Fatal(e)
		}
		if s.SHA256 != testSnapshot(t).SHA256 {
			t.Fatal("unknown identity affects hash")
		}
	})
}

func TestEnumCompositeAndRangeDefinitionsAreUnambiguous(t *testing.T) {
	base := map[string]any{"type": "e", "definition": "[]", "owner": "123", "acl": []any{}, "enum": `["a\nb"]`}
	normal := func(v map[string]any) map[string]string {
		b, _ := json.Marshal(v)
		var raw map[string]json.RawMessage
		json.Unmarshal(b, &raw)
		out, e := normalize("types", "known_type", raw, symbols{"123", "456"}, map[string]int64{})
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	a := normal(base)
	base["enum"] = `["a", "b"]`
	b := normal(base)
	if a["enum_sha256"] == b["enum_sha256"] {
		t.Fatal("enum boundaries collide")
	}
	for _, shape := range []string{`[[1,"field","text"]]`, `[[1,"field","integer"]]`, `["public.range","public.multirange","integer","","pg_catalog.int4_ops","-","-"]`, `["public.range","public.multirange","bigint","","pg_catalog.int8_ops","-","-"]`} {
		base["definition"] = shape
		next := normal(base)
		if next["definition_sha256"] == b["definition_sha256"] {
			t.Fatal("changed type shape invisible")
		}
		b = next
	}
	var sql string
	for _, q := range queries {
		if q.kind == "types" {
			sql = q.sql
		}
	}
	for _, required := range []string{"jsonb_agg(e.enumlabel ORDER BY e.enumsortorder)", "a.attrelid=t.typrelid", "pg_catalog.pg_range", "r.rngsubtype", "r.rngcollation", "r.rngsubopc", "r.rngcanonical", "r.rngsubdiff"} {
		if !strings.Contains(sql, required) {
			t.Fatal("type shape query missing", required)
		}
	}
}

func TestColumnACLAndGrantorChangeAreBound(t *testing.T) {
	base := map[string]any{"position": 1, "type": "text", "not_null": false, "default": "", "identity": "", "generated": "", "collation": "pg_catalog.default", "acl": []any{}}
	normal := func(v map[string]any) map[string]string {
		b, _ := json.Marshal(v)
		var raw map[string]json.RawMessage
		json.Unmarshal(b, &raw)
		out, e := normalize("columns", "value", raw, symbols{"123", "456"}, map[string]int64{})
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	a := normal(base)
	base["acl"] = []any{map[string]any{"grantee": "456", "grantor": "123", "privilege": "SELECT", "grantable": false}}
	b := normal(base)
	if a["acl"] == b["acl"] || b["acl"] != "MIGRATION_OWNER>RUNTIME:SELECT:false" {
		t.Fatal("column privilege invisible")
	}
	base["acl"] = []any{map[string]any{"grantee": "456", "grantor": "456", "privilege": "SELECT", "grantable": false}}
	if normal(base)["acl"] == b["acl"] {
		t.Fatal("grantor invisible")
	}
	for _, q := range queries {
		if q.kind == "columns" && !strings.Contains(q.sql, "a.attacl") {
			t.Fatal("column ACL not collected")
		}
	}
}

func TestIncompleteGoldenRefusedAndDiscoveryExplicit(t *testing.T) {
	c := Catalog{Version: 0, Objects: []Object{{Kind: "relations", Identity: "organizations", Attributes: map[string]string{}}}}
	if _, e := CanonicalCatalog(c); e == nil {
		t.Fatal("incomplete golden exported")
	}
	b, _ := json.Marshal(c)
	if _, e := LoadCatalog(b); e == nil {
		t.Fatal("incomplete golden loaded")
	}
	if _, e := mergedCatalog(c, nil); e != nil {
		t.Fatal("explicit synthetic identity input refused", e)
	}
	for _, field := range []string{"version", "objects"} {
		var v map[string]any
		good, _ := CanonicalCatalog(testCatalog())
		json.Unmarshal(good, &v)
		delete(v, field)
		bad, _ := json.Marshal(v)
		if _, e := LoadCatalog(bad); e == nil {
			t.Fatal("missing required catalog field")
		}
	}
}

func TestSeedByteBudgetBoundsBeforeAllocation(t *testing.T) {
	total := maxSeedBytes - 9
	if e := consumeSeedBudget(&total, "a"); e != nil || total != maxSeedBytes {
		t.Fatal("exact budget")
	}
	requireError(t, consumeSeedBudget(&total, ""), "seed-limit")
	total = 0
	key := strings.Repeat("\x00", 16384)
	accepted := 0
	for consumeSeedBudget(&total, key) == nil {
		accepted++
	}
	if accepted < 1 || accepted > 100 || total > maxSeedBytes {
		t.Fatal("cumulative byte bound")
	}
	if _, e := connectionConfig("postgres://runtime:secret@localhost/db?options=-c%20search_path%3Dpublic", time.Second); e == nil {
		t.Fatal("startup options must not override safety params")
	}
}

func TestPartitionShapeAndStructuredListsAreBound(t *testing.T) {
	base := testSnapshot(t)
	for _, key := range []string{"partition_key_sha256", "partition_bound_sha256", "parents_sha256"} {
		o := testObject()
		o.Attributes[key] = digest([]byte("changed"))
		s, e := project([]Object{o}, testCatalog(), Ownership{}, map[string]int64{})
		if e != nil || s.SHA256 == base.SHA256 {
			t.Fatal("partition shape invisible", key, e)
		}
	}
	var relation, function, sequence string
	for _, q := range queries {
		switch q.kind {
		case "relations":
			relation = q.sql
		case "functions":
			function = q.sql
		case "sequences":
			sequence = q.sql
		}
	}
	for _, part := range []string{"pg_catalog.pg_get_partkeydef(c.oid)", "pg_catalog.pg_get_expr(c.relpartbound,c.oid,false)", "pg_catalog.pg_inherits", "jsonb_build_array(pn.nspname,p.relname)"} {
		if !strings.Contains(relation, part) {
			t.Fatal("partition query", part)
		}
	}
	if !strings.Contains(function, "pg_catalog.to_jsonb(p.proconfig)") || !strings.Contains(sequence, "jsonb_build_array(rn.nspname,r.relname,a.attname,d.deptype::text)") {
		t.Fatal("list boundary ambiguity")
	}
}

func TestOtherGrantorsCollapseWithoutHashingTheirIdentities(t *testing.T) {
	raw := map[string]json.RawMessage{}
	json.Unmarshal([]byte(`{"position":1,"type":"text","not_null":false,"default":"","identity":"","generated":"","collation":"","acl":[{"grantee":"456","grantor":"701","privilege":"SELECT","grantable":false},{"grantee":"456","grantor":"702","privilege":"SELECT","grantable":false}]}`), &raw)
	counts := map[string]int64{}
	out, e := normalize("columns", "name", raw, symbols{"123", "456"}, counts)
	if e != nil {
		t.Fatal(e)
	}
	if out["acl"] != "OTHER>RUNTIME:SELECT:false" || counts["other_acl_entries"] != 2 {
		t.Fatal("unknown grantors not symbolic/count-only", out, counts)
	}
}
