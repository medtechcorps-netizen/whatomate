package dbcatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func nullableTestBytes(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := nullableCanonical(v, nullableObservationLimit)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func nullableFixtureDocument(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "nullable-column-v1.json"))
	if err != nil {
		t.Fatal("actual PG17 fixture required; root must generate it before this suite")
	}
	var document map[string]json.RawMessage
	if err := nullableDecode(raw, (8<<20)+(4<<10), &document); err != nil {
		t.Fatal(err)
	}
	if len(document) != 5 || string(document["format_version"]) != "1" {
		t.Fatal("wrong fixture schema")
	}
	for _, key := range []string{"operation", "before", "after", "explanation"} {
		if document[key] == nil {
			t.Fatal("incomplete fixture")
		}
	}
	return document
}

func nullableTestOperation(t *testing.T) NullableColumnOperationV1 {
	t.Helper()
	op, err := ParseNullableColumnOperationV1(append(nullableFixtureDocument(t)["operation"], '\n'))
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// Every positive and transition mutation uses the complete actual owned PG17
// fixture. Absence is a failure, never a skip or an invented replacement.
func nullableTestFixture(t *testing.T) (NullableObservationV1, NullableObservationV1) {
	t.Helper()
	doc := nullableFixtureDocument(t)
	before, err := ParseNullableObservationV1(append(doc["before"], '\n'))
	if err != nil {
		t.Fatal(err)
	}
	after, err := ParseNullableObservationV1(append(doc["after"], '\n'))
	if err != nil {
		t.Fatal(err)
	}
	return before, after
}

func nullableTestClone(t *testing.T, v NullableObservationV1) NullableObservationV1 {
	t.Helper()
	var out nullableObservation
	if err := json.Unmarshal(nullableTestBytes(t, v.value), &out); err != nil {
		t.Fatal(err)
	}
	return NullableObservationV1{out}
}

func nullableTestRehash(t *testing.T, v *nullableObservation) {
	t.Helper()
	for i := range v.Snapshot.Objects {
		o := &v.Snapshot.Objects[i]
		if o.Kind == "types" && o.Identity == v.Graph.RowType.Object.Identity {
			o.Attributes["definition_sha256"] = digest([]byte(v.Graph.RowType.DefinitionJSON))
		}
		if o.Kind == "types" && o.Identity == v.Graph.ArrayType.Object.Identity {
			o.Attributes["definition_sha256"] = digest([]byte(v.Graph.ArrayType.DefinitionJSON))
		}
	}
	v.Snapshot.Objects = sortedObjects(v.Snapshot.Objects)
	raw, err := CanonicalCatalog(Catalog{Version: 0, Objects: v.Snapshot.Objects})
	if err != nil {
		t.Fatal(err)
	}
	v.Snapshot.SHA256 = digest(raw)
	v.Graph.CatalogSHA256 = v.Snapshot.SHA256
}

func TestNullableOperationClosedRenderer(t *testing.T) {
	op := nullableTestOperation(t)
	raw, err := RenderNullableColumnV1(op)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "ALTER TABLE ONLY \"public\".\"nullable_sample\" ADD COLUMN \"extra\" pg_catalog.int8;\n" {
		t.Fatal("wrong renderer")
	}
	changes := []func(map[string]any){
		func(v map[string]any) { delete(v, "default") }, func(v map[string]any) { delete(v, "nullable") },
		func(v map[string]any) { v["nullable"] = false }, func(v map[string]any) { v["default"] = "NULL" },
		func(v map[string]any) { v["type"] = "bigint" }, func(v map[string]any) { v["type"] = "public.int8" },
		func(v map[string]any) { v["relation"] = "x\";DROP TABLE organizations;--" }, func(v map[string]any) { v["column"] = "a.b" },
		func(v map[string]any) { v["id"] = "A" }, func(v map[string]any) { v["format_version"] = true },
		func(v map[string]any) { v["format_version"] = 2 }, func(v map[string]any) { v["sql"] = "SELECT 1" },
	}
	for _, change := range changes {
		var v map[string]any
		_ = json.Unmarshal(nullableTestBytes(t, op.value), &v)
		change(v)
		if _, err := ParseNullableColumnOperationV1(nullableTestBytes(t, v)); err == nil {
			t.Fatal("unsafe operation accepted")
		}
	}
	if _, err := RenderNullableColumnV1(NullableColumnOperationV1{}); err == nil {
		t.Fatal("zero operation accepted")
	}
}

func TestNullableStrictFramingAndBounds(t *testing.T) {
	op := nullableTestOperation(t)
	raw := nullableTestBytes(t, op.value)
	for _, bad := range [][]byte{raw[:len(raw)-1], append(append([]byte{}, raw...), raw...), append([]byte(" "), raw...), bytes.Replace(raw, []byte(`"format_version":1`), []byte(`"format_version":1.0`), 1), bytes.Replace(raw, []byte(`"format_version":1`), []byte(`"format_version":1,"format_version":1`), 1), bytes.Repeat([]byte(" "), nullableOperationLimit+1)} {
		if _, err := ParseNullableColumnOperationV1(bad); err == nil {
			t.Fatal("noncanonical operation accepted")
		}
	}
	for _, bad := range []string{strings.Repeat("[", 26) + "0" + strings.Repeat("]", 26), `[1e0]`, `[-0]`, `[9223372036854775808]`, `{"x":1,"x":2}`, `["\ud800"]`, `{"` + strings.Repeat("a", 129) + `":1}`, `"` + strings.Repeat("a", nullableDefinitionLimit+1) + `"`} {
		if _, err := nullableJSON([]byte(bad), nullableObservationLimit); err == nil {
			t.Fatal("unsafe JSON accepted")
		}
	}
	if _, err := ParseNullableObservationV1(bytes.Repeat([]byte(" "), nullableObservationLimit+1)); err == nil {
		t.Fatal("oversized observation accepted")
	}
	if _, err := nullableDefinition(strings.Repeat(" ", nullableDefinitionLimit+1), true); err == nil {
		t.Fatal("oversized definition accepted")
	}
}

func TestNullableCompleteTransitionAndCanonicalRoundTrip(t *testing.T) {
	op := nullableTestOperation(t)
	before, after := nullableTestFixture(t)
	raw, err := CanonicalNullableObservationV1(before)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`MIGRATION_OWNER\u003eRUNTIME`)) || !bytes.Contains(raw, []byte(`"parent":""`)) {
		t.Fatal("escaping or graph refs changed")
	}
	parsed, err := ParseNullableObservationV1(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.value, before.value) {
		t.Fatal("roundtrip changed values")
	}
	result, err := ExplainNullableColumnV1(op, parsed, after)
	if err != nil {
		t.Fatal(err)
	}
	output, err := CanonicalNullableExplanationV1(result)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output, append(nullableFixtureDocument(t)["explanation"], '\n')) {
		t.Fatal("actual fixture explanation differs")
	}
	var keys map[string]any
	_ = json.Unmarshal(output, &keys)
	if len(keys) != 8 || len(result.value.Explained) != 2 || result.value.Explained[0].Kind != "columns" || result.value.Explained[1].Kind != "types" {
		t.Fatal("explanation shape differs")
	}
	measured, err := CanonicalCatalog(Catalog{Version: 0, Objects: before.value.Snapshot.Objects})
	if err != nil {
		t.Fatal(err)
	}
	if digest(measured) != before.value.Snapshot.SHA256 || digest(raw) == before.value.Snapshot.SHA256 {
		t.Fatal("catalog hash redefined")
	}
	raw[0] = '!'
	again, err := CanonicalNullableObservationV1(before)
	if err != nil || again[0] != '{' {
		t.Fatal("returned bytes alias opaque value")
	}
}

func TestNullableSnapshotAndGraphRefusals(t *testing.T) {
	before, _ := nullableTestFixture(t)
	cases := map[string]func(*nullableObservation){
		"major": func(v *nullableObservation) { v.PostgresMajor = 14 }, "nil unknown": func(v *nullableObservation) { v.Snapshot.Unknown = nil },
		"unknown":       func(v *nullableObservation) { v.Snapshot.Unknown = []UnknownCount{{Kind: "types", Count: 1}} },
		"missing count": func(v *nullableObservation) { delete(v.Snapshot.CountOnly, "statistics") }, "extra count": func(v *nullableObservation) { v.Snapshot.CountOnly["other"] = 0 },
		"owner": func(v *nullableObservation) { v.Snapshot.Ownership.MigrationOwnerOwnsDatabase = false }, "digest": func(v *nullableObservation) { v.Snapshot.SHA256 = strings.Repeat("0", 64) },
		"reciprocal": func(v *nullableObservation) { v.Graph.ArrayType.Element.Identity = "other" }, "partition": func(v *nullableObservation) { v.Graph.Relation.IsPartition = true },
		"temporary": func(v *nullableObservation) { v.Graph.Relation.Persistence = "t" }, "inheritance": func(v *nullableObservation) { v.Graph.Relation.Children = []nullableRef{{"relations", "child", ""}} },
		"typed": func(v *nullableObservation) { v.Graph.Relation.OfType = &nullableRef{"types", "shape", ""} }, "dropped": func(v *nullableObservation) { v.Graph.Slots[0].Dropped = true },
		"inherited": func(v *nullableObservation) { v.Graph.Slots[0].InheritedCount = 1 }, "nonlocal": func(v *nullableObservation) { v.Graph.Slots[0].Local = false },
		"attnum gap": func(v *nullableObservation) { v.Graph.Slots[0].Number = 2 }, "foreign type": func(v *nullableObservation) { v.Graph.Slots[0].TypeNamespace = "foreign" },
		"unsupported collation": func(v *nullableObservation) { v.Graph.Slots[1].CollationName = "c" }, "hidden slot": func(v *nullableObservation) { v.Graph.Slots = v.Graph.Slots[:1] },
		"raw definition": func(v *nullableObservation) { v.Graph.RowType.DefinitionJSON = "[]" },
		"default ACL": func(v *nullableObservation) {
			v.Graph.Security.DefaultACLCount = 1
			v.Snapshot.CountOnly["default_acls"] = 1
		},
		"foreign ACL": func(v *nullableObservation) {
			v.Graph.Security.OtherACLEntries = 1
			v.Snapshot.CountOnly["other_acl_entries"] = 1
		},
		"foreign policy": func(v *nullableObservation) {
			v.Graph.Security.OtherPolicyRoles = 1
			v.Snapshot.CountOnly["other_policy_roles"] = 1
		},
		"too many slots": func(v *nullableObservation) { v.Graph.Slots = make([]nullableSlot, nullableSlotLimit+1) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			v := nullableTestClone(t, before)
			change(&v.value)
			if _, err := ParseNullableObservationV1(nullableTestBytes(t, v.value)); err == nil {
				t.Fatal("unsafe observation accepted")
			}
		})
	}
	var value map[string]any
	_ = json.Unmarshal(nullableTestBytes(t, before.value), &value)
	delete(value["graph"].(map[string]any)["relation"].(map[string]any), "is_partition")
	if _, err := ParseNullableObservationV1(nullableTestBytes(t, value)); err == nil {
		t.Fatal("missing false field accepted")
	}
}

func TestNullableUnexplainedCompleteChangesRefuse(t *testing.T) {
	op := nullableTestOperation(t)
	before, after := nullableTestFixture(t)
	cases := map[string]func(*nullableObservation){
		"old default": func(v *nullableObservation) {
			for i := range v.Snapshot.Objects {
				if v.Snapshot.Objects[i].Identity == "nullable_sample.label" {
					v.Snapshot.Objects[i].Attributes["default_sha256"] = digest([]byte("'changed'::text"))
				}
			}
		},
		"constraint": func(v *nullableObservation) {
			for i := range v.Snapshot.Objects {
				if v.Snapshot.Objects[i].Kind == "constraints" {
					v.Snapshot.Objects[i].Attributes["definition_sha256"] = digest([]byte("PRIMARY KEY(label)"))
				}
			}
		},
		"relation ACL": func(v *nullableObservation) {
			for i := range v.Snapshot.Objects {
				if v.Snapshot.Objects[i].Kind == "relations" {
					v.Snapshot.Objects[i].Attributes["acl"] = ""
				}
			}
		},
		"database ACL": func(v *nullableObservation) { v.Graph.Security.DatabaseACL = digest([]byte("changed")) }, "schema ACL": func(v *nullableObservation) { v.Graph.Security.PublicSchemaACL = digest([]byte("changed")) },
		"counts": func(v *nullableObservation) { v.Snapshot.CountOnly["statistics"]++ },
		"array definition": func(v *nullableObservation) {
			v.Graph.ArrayType.DefinitionJSON = strings.Replace(v.Graph.ArrayType.DefinitionJSON, `"x"`, `"e"`, 1)
		},
		"row header": func(v *nullableObservation) {
			v.Graph.RowType.DefinitionJSON = strings.Replace(v.Graph.RowType.DefinitionJSON, `"record_out(record)"`, `"other(record)"`, 1)
		},
		"missing unrelated": func(v *nullableObservation) {
			for i, o := range v.Snapshot.Objects {
				if o.Kind == "constraints" {
					v.Snapshot.Objects = append(v.Snapshot.Objects[:i], v.Snapshot.Objects[i+1:]...)
					break
				}
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			v := nullableTestClone(t, after)
			change(&v.value)
			nullableTestRehash(t, &v.value)
			if _, err := CanonicalNullableObservationV1(v); err != nil {
				t.Fatalf("mutation must remain a complete observation: %v", err)
			}
			if _, err := ExplainNullableColumnV1(op, before, v); err == nil {
				t.Fatal("unexplained change accepted")
			}
		})
	}
}

func TestNullableNewColumnRulesNotJustHashEquality(t *testing.T) {
	op := nullableTestOperation(t)
	before, after := nullableTestFixture(t)
	for name, change := range map[string]func(*nullableSlot){
		"default null": func(s *nullableSlot) { s.DefaultPresent = true }, "not null": func(s *nullableSlot) { s.NotNull = true },
		"identity": func(s *nullableSlot) { s.Identity = "d" }, "generated": func(s *nullableSlot) { s.Generated = "s" },
		"domain": func(s *nullableSlot) {
			s.TypeNamespace = "public"
			s.TypeName = "domain_int"
			s.TypeText = "public.domain_int"
		},
		"typmod": func(s *nullableSlot) { s.TypeModifier = 3 }, "ACL": func(s *nullableSlot) { s.ACL = "MIGRATION_OWNER>RUNTIME:SELECT:false" },
	} {
		t.Run(name, func(t *testing.T) {
			v := nullableTestClone(t, after)
			n := len(v.value.Graph.Slots) - 1
			slot := &v.value.Graph.Slots[n]
			change(slot)
			for i := range v.value.Snapshot.Objects {
				o := &v.value.Snapshot.Objects[i]
				if o.Identity == "nullable_sample.extra" {
					o.Attributes["not_null"] = strconv.FormatBool(slot.NotNull)
					o.Attributes["identity"] = slot.Identity
					o.Attributes["generated"] = slot.Generated
					o.Attributes["type_sha256"] = digest([]byte(slot.TypeText))
					o.Attributes["acl"] = slot.ACL
					if slot.DefaultPresent {
						o.Attributes["default_sha256"] = digest([]byte("NULL::bigint"))
					}
				}
			}
			d, err := nullableDefinition(v.value.Graph.RowType.DefinitionJSON, true)
			if err != nil {
				t.Fatal(err)
			}
			f := d[12].([]any)[n].([]any)
			f[2], f[3] = slot.TypeText, slot.NotNull
			raw, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			v.value.Graph.RowType.DefinitionJSON = string(raw)
			nullableTestRehash(t, &v.value)
			if _, err := CanonicalNullableObservationV1(v); err != nil {
				t.Fatalf("mutation must remain internally bound: %v", err)
			}
			if _, err := ExplainNullableColumnV1(op, before, v); err == nil {
				t.Fatal("unsupported column accepted")
			}
		})
	}
}

func TestNullableRawDefinitionAndCanonicalEscapes(t *testing.T) {
	raw, err := nullableCanonical(map[string]string{"s": "<&>\u2028\u2029\u00e9"}, 1000)
	if err != nil || string(raw) != "{\"s\":\"\\u003c\\u0026\\u003e\\u2028\\u2029\u00e9\"}\n" {
		t.Fatal("new canonical escaping changed")
	}
	before, after := nullableTestFixture(t)
	if !strings.Contains(before.value.Graph.RowType.DefinitionJSON, ", ") {
		t.Fatal("actual PG definition spacing lost")
	}
	if _, err := ExplainNullableColumnV1(nullableTestOperation(t), before, after); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(before.value.Graph.RowType.DefinitionJSON, `"C"`, `"A"`, 1), strings.Replace(before.value.Graph.RowType.DefinitionJSON, `false`, `true`, 1), strings.Replace(before.value.Graph.RowType.DefinitionJSON, `-1`, `32768`, 1),
		`["c","C",false,"-","",-1,false,"d","x","in","out","-",[[true,"x","bigint",false,""]],[]]`,
		`["c","C",false,"-","",-1,false,"d","x","in","out","-",[[1,"x","bigint",false,""]],{}]`,
	} {
		if _, err := nullableDefinition(bad, true); err == nil {
			t.Fatal("invalid raw definition accepted")
		}
	}
	if _, err := CanonicalNullableExplanationV1(NullableExplanationV1{}); err == nil {
		t.Fatal("zero explanation accepted")
	}
}

func TestNullableCompleteParentInventory(t *testing.T) {
	before, _ := nullableTestFixture(t)
	for _, empty := range []bool{false, true} {
		v := nullableTestClone(t, before)
		for i := range v.value.Snapshot.Objects {
			o := &v.value.Snapshot.Objects[i]
			if o.Kind == "constraints" {
				if empty {
					o.Parent = ""
					o.Identity = "orphan"
				} else {
					o.Parent = "absent"
					o.Identity = "absent.object"
				}
			}
		}
		nullableTestRehash(t, &v.value)
		if _, err := CanonicalNullableObservationV1(v); err == nil {
			t.Fatal("missing catalog parent accepted")
		}
	}
}

func TestNullableSecurityNormalizationRefusesUnknownAndDuplicates(t *testing.T) {
	roles := symbols{"10", "20"}
	raw := `[{"grantee":"20","grantor":"10","privilege":"SELECT","grantable":false}]`
	got, err := nullableACL(raw, roles)
	if err != nil || got != "MIGRATION_OWNER>RUNTIME:SELECT:false" {
		t.Fatal("symbolic ACL failed")
	}
	for _, bad := range []string{strings.Replace(raw, `"20"`, `"30"`, 1), strings.Replace(raw, `"10"`, `"30"`, 1), strings.TrimSuffix(raw, "]") + "," + strings.TrimPrefix(raw, "["), strings.Replace(raw, "SELECT", "UNKNOWN", 1), `null`} {
		if _, err := nullableACL(bad, roles); err == nil {
			t.Fatal("unsafe ACL normalized away")
		}
	}
}

func TestNullableCaptureRejectsInvalidInputBeforeConnection(t *testing.T) {
	if _, err := CaptureNullableGraphV1(context.Background(), "private-invalid-dsn", Options{}, NullableColumnOperationV1{}); err == nil || err.Error() != "catalog-snapshot:nullable-operation" {
		t.Fatal("zero input reached connection")
	}
	if _, err := CaptureNullableGraphV1(context.Background(), "private-invalid-dsn", Options{IncludeSeeds: true}, nullableTestOperation(t)); err == nil || err.Error() != "catalog-snapshot:nullable-capture" {
		t.Fatal("seed option reached connection")
	}
}
