package dbcatalog

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func planSourceWith(t *testing.T, steps, bindings, globals, history []byte) PlanSource {
	t.Helper()
	source, err := decodePlanSource(builtinCatalog, nullableBuiltinCatalog, bootstrapEmptyProductionV0, globals, history, steps, bindings, false)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestPlanSourcesSelectCompleteFixedCatalogsAndCloneMaps(t *testing.T) {
	golden, err := BuiltinGoldenPlanInputs()
	if err != nil {
		t.Fatal(err)
	}
	production, err := BuiltinProductionPlanInputs()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		source PlanSource
		hash   string
	}{{golden, planGoldenSHA256}, {production, nullableBuiltinCatalogSHA256}} {
		first := test.source.CaptureCatalog()
		raw, err := CanonicalCatalog(first)
		if err != nil || digest(raw) != test.hash {
			t.Fatal("wrong fixed catalog", err)
		}
		first.Objects[0].Identity = "changed"
		for k := range first.Objects[0].Attributes {
			first.Objects[0].Attributes[k] = "changed"
			break
		}
		second := test.source.CaptureCatalog()
		raw, err = CanonicalCatalog(second)
		if err != nil || digest(raw) != test.hash {
			t.Fatal("shared slice/map backing store", err)
		}
	}
	if golden.otherSchemas != nil || production.otherSchemas == nil || *production.otherSchemas != 7 {
		t.Fatal("source-specific schema threshold")
	}
	if len(golden.unknown) != 0 || len(production.unknown) != 0 {
		t.Fatal("unknown baseline learned from live facts")
	}
	if len(golden.steps) != 21 {
		t.Fatal("incomplete declaration inventory")
	}
	// A new constructor decodes afresh; no mutable singleton storage is returned.
	golden.catalog.Objects[0].Attributes["changed"] = "changed"
	again, err := BuiltinGoldenPlanInputs()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := CanonicalCatalog(again.CaptureCatalog())
	if err != nil || digest(raw) != planGoldenSHA256 {
		t.Fatal("cached mutable source", err)
	}
}

func TestPlanRegistryStrictRequiredTypesAndClosedSemantics(t *testing.T) {
	edits := map[string]func([]map[string]any){
		"missing boolean":     func(r []map[string]any) { delete(r[0], "baseline") },
		"null boolean":        func(r []map[string]any) { r[0]["baseline"] = nil },
		"false baseline":      func(r []map[string]any) { r[0]["baseline"] = false },
		"null gates":          func(r []map[string]any) { r[0]["requires_gates"] = nil },
		"unknown gate":        func(r []map[string]any) { r[0]["requires_gates"] = []string{"future/gate.json"} },
		"fractional revision": func(r []map[string]any) { r[0]["rev"] = 1.5 },
		"boolean revision":    func(r []map[string]any) { r[0]["rev"] = true },
		"zero revision":       func(r []map[string]any) { r[0]["rev"] = 0 },
		"null revision":       func(r []map[string]any) { r[0]["rev"] = nil },
		"n1 claim":            func(r []map[string]any) { r[0]["n1_safe"] = true },
		"duplicate id":        func(r []map[string]any) { r[1]["id"] = r[0]["id"] },
		"source order":        func(r []map[string]any) { r[0], r[1] = r[1], r[0] },
		"foreign function":    func(r []map[string]any) { r[0]["func"] = "unsafe.Execute" },
		"ledger kind":         func(r []map[string]any) { r[0]["kind"] = "ledger-managed" },
		"extra key":           func(r []map[string]any) { r[0]["authority"] = true },
		"empty path":          func(r []map[string]any) { r[0]["closure_files"] = []string{} },
		"path traversal":      func(r []map[string]any) { r[0]["closure_files"] = []string{"../x.go"} },
		"absolute path":       func(r []map[string]any) { r[0]["closure_files"] = []string{"/x.go"} },
		"windows path":        func(r []map[string]any) { r[0]["closure_files"] = []string{`C:\x.go`} },
		"test path":           func(r []map[string]any) { r[0]["closure_files"] = []string{"x_test.go"} },
		"unsorted paths":      func(r []map[string]any) { r[0]["closure_files"] = []string{"b.go", "a.go"} },
		"duplicate paths":     func(r []map[string]any) { r[0]["closure_files"] = []string{"a.go", "a.go"} },
		"bad hash":            func(r []map[string]any) { r[0]["closure_sha256"] = strings.Repeat("A", 64) },
		"dangling parent":     func(r []map[string]any) { r[8]["parent"] = "absent" },
		"empty parent":        func(r []map[string]any) { r[0]["parent"] = "" },
		"null parent":         func(r []map[string]any) { r[0]["parent"] = nil },
		"missing parent":      func(r []map[string]any) { delete(r[8], "parent") },
		"parent idempotent":   func(r []map[string]any) { r[7]["idempotent"] = true },
		"legacy writable":     func(r []map[string]any) { r[12]["idempotent"] = false },
		"split revision":      func(r []map[string]any) { r[12]["rev"] = r[7]["rev"].(float64) + 1 },
		"old pair":            func(r []map[string]any) { r[7]["rev"] = 1; r[12]["rev"] = 1 },
		"split closure":       func(r []map[string]any) { r[12]["closure_sha256"] = strings.Repeat("0", 64) },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			var rows []map[string]any
			if err := json.Unmarshal(builtinPlanSteps, &rows); err != nil {
				t.Fatal(err)
			}
			edit(rows)
			raw, err := json.Marshal(rows)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = decodePlanSteps(raw); err == nil {
				t.Fatal("malformed declaration accepted")
			}
		})
	}
	for name, raw := range map[string][]byte{
		"duplicate key": bytes.Replace(builtinPlanSteps, []byte(`"baseline": true`), []byte(`"baseline": true, "baseline": true`), 1),
		"trailing":      append(append([]byte(nil), builtinPlanSteps...), []byte(` []`)...),
		"null":          []byte(`null`), "empty": []byte(`[]`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodePlanSteps(raw); err == nil {
				t.Fatal("invalid array accepted")
			}
		})
	}
}

func TestPlanRegistryAllowsMeasuredRevisionAdvanceWithoutChangingRemovalContract(t *testing.T) {
	var rows []map[string]any
	if err := json.Unmarshal(builtinPlanSteps, &rows); err != nil {
		t.Fatal(err)
	}
	revision := rows[7]["rev"].(float64) + 1
	rows[7]["rev"] = revision
	rows[12]["rev"] = revision
	rows[0]["rev"] = rows[0]["rev"].(float64) + 1
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	source := planSourceWith(t, raw, builtinPlanBindings, builtinGlobals, builtinHistory)
	if source.steps[7].Rev != int(revision) || source.steps[12].Rev != int(revision) {
		t.Fatal("genuine paired revision advance frozen out")
	}
}

func TestPlanSourceBindsFourExecutableChildrenAndInertRemovedWrite(t *testing.T) {
	edits := map[string]func(map[string]any){
		"legacy executed": func(v map[string]any) {
			v["seed_child_calls"] = append(v["seed_child_calls"].([]any), "legacy-super-admin-update")
		},
		"child omitted":       func(v map[string]any) { v["seed_child_order"] = v["seed_child_order"].([]any)[:3] },
		"inline sql restored": func(v map[string]any) { v["inline_sql"] = "UPDATE users SET is_super_admin=true" },
		"rationale lost":      func(v map[string]any) { delete(v["idempotency_rationale"].(map[string]any), planLegacyID) },
		"removal unbound": func(v map[string]any) {
			v["idempotency_rationale"].(map[string]any)[planLegacyID] = "any revision is safe"
		},
		"child nullable": func(v map[string]any) { v["seed_child_calls"] = nil },
		"extra":          func(v map[string]any) { v["skip_checks"] = true },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			var v map[string]any
			if err := json.Unmarshal(builtinPlanBindings, &v); err != nil {
				t.Fatal(err)
			}
			edit(v)
			raw, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if err = validatePlanBindings(raw); err == nil {
				t.Fatal("removed-write contract not enforced")
			}
		})
	}
	if err := validatePlanBindings(builtinPlanBindings); err != nil {
		t.Fatal(err)
	}
}

func TestPlanSourceRejectsUnsupportedMaterialWithoutFallback(t *testing.T) {
	original := [][]byte{builtinCatalog, nullableBuiltinCatalog, bootstrapEmptyProductionV0, builtinGlobals, builtinHistory, builtinPlanSteps, builtinPlanBindings}
	for i := range original {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			input := append([][]byte(nil), original...)
			input[i] = []byte(`null`)
			for _, production := range []bool{false, true} {
				if _, err := decodePlanSource(input[0], input[1], input[2], input[3], input[4], input[5], input[6], production); err == nil {
					t.Fatal("unsupported material fell back")
				}
			}
		})
	}
	for _, history := range []string{`[{"version":null,"class":"baseline","min_reader_version":0}]`, `[{"version":0,"class":"baseline"}]`, `[{"version":0,"class":"baseline","min_reader_version":1}]`, `[{"version":0,"class":"future","min_reader_version":0}]`} {
		if _, err := decodePlanSource(builtinCatalog, nullableBuiltinCatalog, bootstrapEmptyProductionV0, builtinGlobals, []byte(history), builtinPlanSteps, builtinPlanBindings, false); err == nil {
			t.Fatal("future/malformed history accepted")
		}
	}
	for _, globals := range []string{`[{"name":"permissions","reason":null}]`, `[{"name":"permissions","reason":"global definitions","keys":null}]`} {
		if _, err := decodePlanSource(builtinCatalog, nullableBuiltinCatalog, bootstrapEmptyProductionV0, []byte(globals), builtinHistory, builtinPlanSteps, builtinPlanBindings, false); err == nil {
			t.Fatal("nullable globals accepted")
		}
	}
	if _, err := decodePlanSource(builtinCatalog, nullableBuiltinCatalog, bootstrapEmptyProductionV0, builtinGlobals, builtinHistory, bytes.Repeat([]byte(" "), (256<<10)+1), builtinPlanBindings, false); err == nil {
		t.Fatal("registry cap ignored")
	}
	// All ten seed declarations are exactly the observation order, with no
	// executable callback exposed by PlanSource.
	source := planSourceWith(t, builtinPlanSteps, builtinPlanBindings, builtinGlobals, builtinHistory)
	var ids []string
	for _, row := range source.steps {
		if row.Kind == "seed-reconciler" {
			ids = append(ids, row.ID)
		}
	}
	expected := planSeedIDs()
	if !reflect.DeepEqual(ids, expected[:]) {
		t.Fatal("seed coverage differs from declaration")
	}
}
