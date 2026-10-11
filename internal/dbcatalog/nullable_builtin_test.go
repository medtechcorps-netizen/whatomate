package dbcatalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type nullableProfileDelta struct {
	Kind        string `json:"kind"`
	Identity    string `json:"identity"`
	Disposition string `json:"disposition"`
	Attributes  map[string]struct {
		Golden     string `json:"golden"`
		Production string `json:"production"`
	} `json:"attributes"`
}

type nullableSourceProfile struct {
	Version             int                    `json:"version"`
	LiveSHA256          string                 `json:"live_sha256"`
	GoldenSHA256        string                 `json:"golden_sha256"`
	ReleasedSource      string                 `json:"released_source"`
	Delta               []nullableProfileDelta `json:"delta"`
	AcceptedUnpublished []UnknownCount         `json:"accepted_unpublished"`
	OtherSchemas        int                    `json:"n_other_schemas"`
	ColumnOrder         []struct {
		Table   string   `json:"table"`
		Columns []string `json:"columns"`
	} `json:"bootstrap_column_order"`
}

// Only existing source files supply the expectation. No observation fixture is
// read here, and every old value is checked before a production value is used.
func nullableBaselineSources(t *testing.T) ([]byte, nullableSourceProfile) {
	t.Helper()
	golden, err := os.ReadFile(filepath.Join("golden", "catalog.json"))
	if err != nil || digest(golden) != "17ae6791a73745f3e34ff30760aced9f27b98e189eace34f108ff1d8b93465b8" {
		t.Fatal("original source golden required")
	}
	raw, err := os.ReadFile(filepath.Join("golden", "production-v0.json"))
	if err != nil || digest(raw) != "808e11a5fdab638adc6f0004e9595371d1eee9a51763e12e779b355f7cdd391d" {
		t.Fatal("original reviewed production profile required")
	}
	var profile nullableSourceProfile
	if strictDecode(raw, &profile) != nil {
		t.Fatal("invalid source profile")
	}
	return golden, profile
}

func nullableReconstructSource(golden []byte, p nullableSourceProfile) (Catalog, error) {
	bad := errors.New("source reconstruction refused")
	if p.Version != 0 || p.GoldenSHA256 != digest(golden) || p.GoldenSHA256 != "17ae6791a73745f3e34ff30760aced9f27b98e189eace34f108ff1d8b93465b8" || p.LiveSHA256 != nullableBuiltinCatalogSHA256 || p.ReleasedSource != "e0016b3595114728e7864da2adb44c29065f25ef" || p.AcceptedUnpublished == nil || len(p.AcceptedUnpublished) != 0 || p.OtherSchemas != 7 || len(p.Delta) != 72 || len(p.ColumnOrder) != 8 {
		return Catalog{}, bad
	}
	catalog, err := LoadCatalog(golden)
	if err != nil || len(catalog.Objects) != 4432 {
		return Catalog{}, bad
	}
	indices := map[string]int{}
	for i, object := range catalog.Objects {
		key := object.Kind + ":" + object.Identity
		if _, exists := indices[key]; exists {
			return Catalog{}, bad
		}
		indices[key] = i
	}
	seen := map[string]bool{}
	for _, row := range p.Delta {
		key := row.Kind + ":" + row.Identity
		index, ok := indices[key]
		if !ok || seen[key] || row.Disposition != "fix-bootstrap" || len(row.Attributes) == 0 {
			return Catalog{}, bad
		}
		seen[key] = true
		for name, change := range row.Attributes {
			old, ok := catalog.Objects[index].Attributes[name]
			if !ok || old != change.Golden || old == change.Production {
				return Catalog{}, bad
			}
			catalog.Objects[index].Attributes[name] = change.Production
		}
	}
	wire, err := CanonicalCatalog(catalog)
	if err != nil || digest(wire) != nullableBuiltinCatalogSHA256 {
		return Catalog{}, bad
	}
	return catalog, nil
}

func TestNullableBuiltinCatalogIsIndependentCompleteSourceReconstruction(t *testing.T) {
	golden, profile := nullableBaselineSources(t)
	expected, err := nullableReconstructSource(golden, profile)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := CanonicalCatalog(expected)
	if err != nil || !bytes.Equal(wire, nullableBuiltinCatalog) || len(wire) != 1762225 {
		t.Fatal("embedded baseline differs from independently reconstructed complete source")
	}
	actual, operation, err := nullableBuiltinInputs(nullableBuiltinCatalog, nullableBuiltinOperation)
	if err != nil || !reflect.DeepEqual(actual, expected) || operation.value.Relation != "organizations" || operation.value.Column != "extra" {
		t.Fatal("fixed embedded inputs changed")
	}
	if digest(nullableBuiltinOperation) != "c775d31243e85e933598b06d3828cc329460902b27d1f0f1a9b2b62a022424d2" {
		t.Fatal("fixed probe declaration changed")
	}
}

func TestNullableBuiltinSourceReconstructionRejectsIncompleteOrForeignDeltas(t *testing.T) {
	for _, name := range []string{"omit", "duplicate", "unknown-object", "unknown-attribute", "wrong-old", "unchanged", "disposition", "coherent-wrong-target", "source", "version", "unpublished", "golden-hash"} {
		t.Run(name, func(t *testing.T) {
			golden, profile := nullableBaselineSources(t)
			switch name {
			case "omit":
				profile.Delta = profile.Delta[1:]
			case "duplicate":
				profile.Delta[1] = profile.Delta[0]
			case "unknown-object":
				profile.Delta[0].Identity = "absent.column"
			case "unknown-attribute":
				profile.Delta[0].Attributes["unsupported"] = profile.Delta[0].Attributes["position"]
			case "wrong-old", "unchanged", "coherent-wrong-target":
				change := profile.Delta[0].Attributes["position"]
				if name == "wrong-old" {
					change.Golden = "999"
				} else if name == "unchanged" {
					change.Production = change.Golden
				} else {
					change.Production = "999"
				}
				profile.Delta[0].Attributes["position"] = change
			case "disposition":
				profile.Delta[0].Disposition = "learn-database"
			case "source":
				profile.ReleasedSource = strings.Repeat("a", 40)
			case "version":
				profile.Version = 1
			case "unpublished":
				profile.AcceptedUnpublished = append(profile.AcceptedUnpublished, UnknownCount{Kind: "relations", Count: 1})
			case "golden-hash":
				profile.GoldenSHA256 = nullableBuiltinCatalogSHA256
			}
			if _, err := nullableReconstructSource(golden, profile); err == nil {
				t.Fatal("incomplete or foreign source reconstruction accepted")
			}
		})
	}
}

func TestNullableBuiltinInputsAreClosedAndBounded(t *testing.T) {
	golden, _ := nullableBaselineSources(t)
	for _, raw := range [][]byte{nil, golden, append(bytes.Clone(nullableBuiltinCatalog), ' '), bytes.Repeat([]byte("x"), nullableObservationLimit+1)} {
		if _, _, err := nullableBuiltinInputs(raw, nullableBuiltinOperation); err == nil {
			t.Fatal("unreviewed catalog accepted")
		}
	}
	for _, replacement := range [][2]string{
		{`"format_version":1`, `"format_version":true`},
		{`"id":"add_extra"`, `"id":"other_probe"`},
		{`"relation":"organizations"`, `"relation":"messages"`},
		{`"column":"extra"`, `"column":"different"`},
		{`"type":"pg_catalog.int8"`, `"type":"pg_catalog.int4"`},
		{`"nullable":true`, `"nullable":false`},
		{`"default":null`, `"default":{}`},
		{`"default":null`, `"default":null,"default":null`},
		{`"column":"extra"`, `"unsupported":false,"column":"extra"`},
	} {
		raw := bytes.Replace(nullableBuiltinOperation, []byte(replacement[0]), []byte(replacement[1]), 1)
		if bytes.Equal(raw, nullableBuiltinOperation) {
			t.Fatal("operation mutation missed its premise")
		}
		if _, _, err := nullableBuiltinInputs(nullableBuiltinCatalog, raw); err == nil {
			t.Fatal("non-fixed operation accepted")
		}
	}
	for _, raw := range [][]byte{nil, append(bytes.Clone(nullableBuiltinOperation), ' '), bytes.Repeat([]byte("x"), nullableOperationLimit+1)} {
		if _, _, err := nullableBuiltinInputs(nullableBuiltinCatalog, raw); err == nil {
			t.Fatal("malformed or oversized operation accepted")
		}
	}
}

func TestNullableBuiltinInputsReturnIndependentMutableCatalogCopies(t *testing.T) {
	first, _, err := nullableBuiltinInputs(nullableBuiltinCatalog, nullableBuiltinOperation)
	if err != nil {
		t.Fatal(err)
	}
	first.Objects[0].Attributes["position"] = "999"
	second, _, err := nullableBuiltinInputs(nullableBuiltinCatalog, nullableBuiltinOperation)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := CanonicalCatalog(second)
	if err != nil || !bytes.Equal(wire, nullableBuiltinCatalog) {
		t.Fatal("a caller mutated later compiled-source expectations")
	}
}

// Controlled synthetic observation, NOT actual production/H qualification.
// First establish expected bytes from independent source. The mandatory actual
// AD fixture then supplies only the graph/security carrier. Reorder its raw
// organizations definition/slots according to the independent source profile,
// checking the resulting type hash against source before validating anything.
func nullableControlledProductionBaseline(t *testing.T) (Catalog, NullableColumnOperationV1, NullableObservationV2) {
	t.Helper()
	golden, profile := nullableBaselineSources(t)
	expected, err := nullableReconstructSource(golden, profile)
	if err != nil {
		t.Fatal(err)
	}
	_, operation, err := nullableBuiltinInputs(nullableBuiltinCatalog, nullableBuiltinOperation)
	if err != nil {
		t.Fatal(err)
	}
	_, observed, _ := nullableV2Fixture(t)
	if observed.value.Snapshot.SHA256 != profile.GoldenSHA256 || digest([]byte(observed.value.Graph.RowType.DefinitionJSON)) != "63697f813c0cfa2b9ae24e6d604bda9d64b9851720c11a9963ba059b824bb1ea" {
		t.Fatal("actual carrier does not match the original independent source")
	}
	definition, err := nullableDefinition(observed.value.Graph.RowType.DefinitionJSON, true)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string][]any{}
	for _, field := range definition[12].([]any) {
		values := field.([]any)
		fields[values[1].(string)] = values
	}
	slots := map[string]nullableSlot{}
	for _, slot := range observed.value.Graph.Slots {
		slots[slot.Name] = slot
	}
	var order []string
	for _, row := range profile.ColumnOrder {
		if row.Table == "organizations" {
			order = row.Columns
		}
	}
	if len(order) != len(fields) || len(order) != len(slots) || len(order) == 0 {
		t.Fatal("source column order is incomplete")
	}
	newFields := []any{}
	newSlots := []nullableSlot{}
	for i, name := range order {
		field, exists := fields[name]
		slot, slotExists := slots[name]
		if !exists || !slotExists {
			t.Fatal("source profile column absent from carrier")
		}
		field[0] = i + 1
		slot.Number = i + 1
		newFields = append(newFields, field)
		newSlots = append(newSlots, slot)
	}
	definition[12] = newFields
	raw, err := json.Marshal(definition)
	// PostgreSQL json_build_array separates elements with comma-space. Restore
	// that framing only outside quoted strings; function signatures such as
	// record_in(cstring,oid,integer) must retain their original bytes.
	if err == nil {
		var framed bytes.Buffer
		quoted, escaped := false, false
		for _, b := range raw {
			framed.WriteByte(b)
			if quoted {
				if escaped {
					escaped = false
				} else if b == '\\' {
					escaped = true
				} else if b == '"' {
					quoted = false
				}
			} else if b == '"' {
				quoted = true
			} else if b == ',' {
				framed.WriteByte(' ')
			}
		}
		raw = framed.Bytes()
	}
	if err != nil || digest(raw) != "5eb86115e47f40c97e52705ae5134b2358e24fb98ee2725c859df61bc5148df4" {
		t.Fatal("reconstructed raw production row type differs from independently pinned source")
	}
	observed.value.Graph.RowType.DefinitionJSON = string(raw)
	observed.value.Graph.Slots = newSlots
	observed.value.Snapshot.Objects = expected.Objects
	observed.value.Snapshot.SHA256 = nullableBuiltinCatalogSHA256
	observed.value.Graph.CatalogSHA256 = nullableBuiltinCatalogSHA256
	if validateNullableObservationV2(observed.value) != nil {
		t.Fatal("controlled source-derived carrier is not internally consistent")
	}
	return expected, operation, observed
}

func TestNullableBuiltinControlledFullBaselineAndActualGoldenSeparation(t *testing.T) {
	catalog, operation, observed := nullableControlledProductionBaseline(t)
	if err := validateNullableBuiltinBaseline(observed, catalog, operation); err != nil {
		t.Fatal(err)
	}
	raw, err := CanonicalNullableObservationV2(observed)
	if err != nil || len(raw) <= 1<<20 || len(raw) > nullableObservationLimit {
		t.Fatal("complete controlled observation exceeds fixed cap or became a projection")
	}
	parsed, err := ParseNullableObservationV2(raw)
	if err != nil || validateNullableBuiltinBaseline(parsed, catalog, operation) != nil {
		t.Fatal("complete controlled baseline did not roundtrip")
	}
	_, before, after := nullableV2Fixture(t)
	for _, actual := range []NullableObservationV2{before, after} {
		if validateNullableBuiltinBaseline(actual, catalog, operation) == nil {
			t.Fatal("actual application golden/transition mislabeled production baseline")
		}
	}
}

func TestNullableBuiltinRejectsStructurallyValidWrongDefaultACLs(t *testing.T) {
	catalog, operation, observed := nullableControlledProductionBaseline(t)
	for _, name := range []string{"grantable", "missing-row", "empty-rows"} {
		t.Run(name, func(t *testing.T) {
			changed := nullableV2Clone(t, observed)
			if name == "grantable" {
				changed.value.Graph.Security.DefaultACLRows[0].ACL[0].Grantable = true
			} else if name == "missing-row" {
				changed.value.Graph.Security.DefaultACLRows = changed.value.Graph.Security.DefaultACLRows[:1]
			} else {
				changed.value.Graph.Security.DefaultACLRows = []nullableDefaultRow{}
			}
			count := int64(len(changed.value.Graph.Security.DefaultACLRows))
			changed.value.Graph.Security.DefaultACLCount = count
			changed.value.Snapshot.CountOnly["default_acls"] = count
			if validateNullableObservationV2(changed.value) != nil {
				t.Fatal("mutation must remain structurally valid to exercise fixed ACL equality")
			}
			if err := validateNullableBuiltinBaseline(changed, catalog, operation); err == nil || err.Error() != "catalog-snapshot:nullable-default-contract" {
				t.Fatal("unexpected default ACL qualified or wrong premise")
			}
		})
	}
}

func TestNullableBuiltinRejectsCompleteCoherentCatalogDrift(t *testing.T) {
	catalog, operation, observed := nullableControlledProductionBaseline(t)
	changed := nullableV2Clone(t, observed)
	changed.value.Snapshot.Objects[0].Attributes["position"] = "999"
	wrong := Catalog{Version: 0, Objects: changed.value.Snapshot.Objects}
	wire, err := CanonicalCatalog(wrong)
	if err != nil {
		t.Fatal(err)
	}
	changed.value.Snapshot.SHA256 = digest(wire)
	changed.value.Graph.CatalogSHA256 = digest(wire)
	if validateNullableObservationV2(changed.value) != nil {
		t.Fatal("coherent unrelated object mutation invalidated its structural premise")
	}
	if validateNullableBuiltinBaseline(changed, catalog, operation) == nil || validateNullableBuiltinBaseline(changed, wrong, operation) == nil {
		t.Fatal("coherent observed/expected drift bypassed fixed source")
	}
}

func TestNullableBuiltinRejectsGraphSecurityAndForgedProbe(t *testing.T) {
	catalog, operation, observed := nullableControlledProductionBaseline(t)
	for _, mutate := range []func(*nullableObservationV2){
		func(v *nullableObservationV2) { v.Graph.Relation.Object.Identity = "messages" },
		func(v *nullableObservationV2) { v.Graph.ArrayType.Element.Identity = "messages" },
		func(v *nullableObservationV2) { v.Graph.Slots[0].Number = 2 },
		func(v *nullableObservationV2) { v.Graph.Security.DatabaseACL = "not-a-hash" },
		func(v *nullableObservationV2) { v.Snapshot.Ownership.MigrationOwnerOwnsDatabase = false },
		func(v *nullableObservationV2) {
			v.Snapshot.Unknown = append(v.Snapshot.Unknown, UnknownCount{Kind: "relations", Count: 1})
		},
	} {
		changed := nullableV2Clone(t, observed)
		mutate(&changed.value)
		if validateNullableBuiltinBaseline(changed, catalog, operation) == nil {
			t.Fatal("graph or security mismatch accepted")
		}
	}
	for _, value := range []any{map[string]any{}, []any{1}, "default"} {
		forged := operation
		forged.value.Default = value
		if validateNullableBuiltinBaseline(observed, catalog, forged) == nil {
			t.Fatal("forged probe accepted")
		}
	}
}
