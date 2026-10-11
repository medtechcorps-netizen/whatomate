package dbcatalog

// This preparatory observer explains one complete catalog transition. It never
// executes its rendered statement or grants source, migration or release authority.
import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const nullableOperationLimit = 2 << 10
const nullableDefinitionLimit = 256 << 10
const nullableGraphLimit = 1 << 20
const nullableObservationLimit = 4 << 20
const nullableSlotLimit = 1024

var nullableOperationID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var nullableInteger = regexp.MustCompile(`^(0|-?[1-9][0-9]*)$`)

// Opaque values retain validated data, not a capability or authenticated proof.
type NullableColumnOperationV1 struct{ value nullableOperation }
type NullableObservationV1 struct{ value nullableObservation }
type NullableExplanationV1 struct{ value nullableExplanation }

type nullableOperation struct {
	FormatVersion int    `json:"format_version"`
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Relation      string `json:"relation"`
	Column        string `json:"column"`
	Type          string `json:"type"`
	Nullable      bool   `json:"nullable"`
	Default       any    `json:"default"`
}

type nullableRef struct {
	Kind     string `json:"kind"`
	Identity string `json:"identity"`
	Parent   string `json:"parent"`
}

type nullableRelation struct {
	Object      nullableRef   `json:"object"`
	Relkind     string        `json:"relkind"`
	Persistence string        `json:"persistence"`
	IsPartition bool          `json:"is_partition"`
	OfType      *nullableRef  `json:"of_type"`
	Parents     []nullableRef `json:"parents"`
	Children    []nullableRef `json:"children"`
	RowType     nullableRef   `json:"row_type"`
}

type nullableRowType struct {
	Object         nullableRef `json:"object"`
	Relation       nullableRef `json:"relation"`
	Array          nullableRef `json:"array"`
	DefinitionJSON string      `json:"definition_json"`
}

type nullableArrayType struct {
	Object         nullableRef `json:"object"`
	Element        nullableRef `json:"element"`
	DefinitionJSON string      `json:"definition_json"`
}

type nullableSlot struct {
	Number             int    `json:"number"`
	Name               string `json:"name"`
	TypeNamespace      string `json:"type_namespace"`
	TypeName           string `json:"type_name"`
	TypeModifier       int    `json:"type_modifier"`
	TypeText           string `json:"type_text"`
	NotNull            bool   `json:"not_null"`
	Dropped            bool   `json:"dropped"`
	Local              bool   `json:"local"`
	InheritedCount     int    `json:"inherited_count"`
	DefaultPresent     bool   `json:"default_present"`
	Identity           string `json:"identity"`
	Generated          string `json:"generated"`
	CollationNamespace string `json:"collation_namespace"`
	CollationName      string `json:"collation_name"`
	ACL                string `json:"acl"`
}

type nullableSecurity struct {
	DatabaseACL      string `json:"database_acl_sha256"`
	PublicSchemaACL  string `json:"public_schema_acl_sha256"`
	DefaultACLCount  int64  `json:"default_acl_count"`
	OtherACLEntries  int64  `json:"other_acl_entries"`
	OtherPolicyRoles int64  `json:"other_policy_roles"`
}

type nullableGraph struct {
	FormatVersion int               `json:"format_version"`
	CatalogSHA256 string            `json:"catalog_sha256"`
	Relation      nullableRelation  `json:"relation"`
	RowType       nullableRowType   `json:"row_type"`
	ArrayType     nullableArrayType `json:"array_type"`
	Slots         []nullableSlot    `json:"slots"`
	Security      nullableSecurity  `json:"security"`
}

type nullableObservation struct {
	FormatVersion int           `json:"format_version"`
	PostgresMajor int           `json:"postgres_major"`
	Snapshot      Snapshot      `json:"snapshot"`
	Graph         nullableGraph `json:"graph"`
}

type nullableExplanation struct {
	FormatVersion           int           `json:"format_version"`
	OperationSHA256         string        `json:"operation_sha256"`
	StatementSHA256         string        `json:"statement_sha256"`
	BeforeCatalogSHA256     string        `json:"before_catalog_sha256"`
	AfterCatalogSHA256      string        `json:"after_catalog_sha256"`
	BeforeObservationSHA256 string        `json:"before_observation_sha256"`
	AfterObservationSHA256  string        `json:"after_observation_sha256"`
	Explained               []nullableRef `json:"explained"`
}

// nullableJSON checks duplicate keys, exact integral numbers and bounded nesting
// before typed decoding. Raw definition strings are checked separately as JSON.
func nullableJSON(raw []byte, limit int) (any, error) {
	if len(raw) == 0 || len(raw) > limit || !utf8.Valid(raw) {
		return nil, refuse("nullable-format")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		nodes++
		if depth > 24 || nodes > 500000 {
			return nil, refuse("nullable-limit")
		}
		t, err := d.Token()
		if err != nil {
			return nil, refuse("nullable-format")
		}
		switch v := t.(type) {
		case json.Delim:
			if v == '{' {
				m := map[string]any{}
				for d.More() {
					key, err := d.Token()
					k, ok := key.(string)
					if err != nil || !ok || len(k) > 128 || strings.ContainsRune(k, utf8.RuneError) {
						return nil, refuse("nullable-format")
					}
					if _, duplicate := m[k]; duplicate {
						return nil, refuse("nullable-format")
					}
					m[k], err = read(depth + 1)
					if err != nil {
						return nil, err
					}
				}
				end, err := d.Token()
				if err != nil || end != json.Delim('}') {
					return nil, refuse("nullable-format")
				}
				return m, nil
			}
			if v == '[' {
				a := []any{}
				for d.More() {
					item, err := read(depth + 1)
					if err != nil {
						return nil, err
					}
					a = append(a, item)
				}
				end, err := d.Token()
				if err != nil || end != json.Delim(']') {
					return nil, refuse("nullable-format")
				}
				return a, nil
			}
			return nil, refuse("nullable-format")
		case json.Number:
			if !nullableInteger.MatchString(v.String()) {
				return nil, refuse("nullable-format")
			}
			if _, err := strconv.ParseInt(v.String(), 10, 64); err != nil {
				return nil, refuse("nullable-format")
			}
		case string:
			if len(v) > nullableDefinitionLimit || strings.ContainsRune(v, utf8.RuneError) {
				return nil, refuse("nullable-format")
			}
		}
		return t, nil
	}
	v, err := read(0)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, refuse("nullable-format")
	}
	return v, nil
}

// New envelopes sort keys recursively. Go JSON string escaping is normative;
// the existing CanonicalCatalog representation is deliberately not changed.
func nullableCanonical(value any, limit int) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, refuse("nullable-format")
	}
	tree, err := nullableJSON(raw, limit)
	if err != nil {
		return nil, err
	}
	raw, err = json.Marshal(tree)
	if err != nil || len(raw)+1 > limit {
		return nil, refuse("nullable-limit")
	}
	return append(raw, '\n'), nil
}

func nullableDecode(raw []byte, limit int, value any) error {
	if _, err := nullableJSON(raw, limit); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil {
		return refuse("nullable-format")
	}
	canonical, err := nullableCanonical(value, limit)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, canonical) {
		return refuse("nullable-format")
	}
	return nil
}

func validateNullableOperation(op nullableOperation) error {
	if op.FormatVersion != 1 || !nullableOperationID.MatchString(op.ID) || op.Kind != "add_nullable_column" || !identifier.MatchString(op.Relation) || !identifier.MatchString(op.Column) || op.Type != "pg_catalog.int8" || !op.Nullable || op.Default != nil {
		return refuse("nullable-operation")
	}
	return nil
}

func ParseNullableColumnOperationV1(raw []byte) (NullableColumnOperationV1, error) {
	var op nullableOperation
	if err := nullableDecode(raw, nullableOperationLimit, &op); err != nil {
		return NullableColumnOperationV1{}, err
	}
	if err := validateNullableOperation(op); err != nil {
		return NullableColumnOperationV1{}, err
	}
	return NullableColumnOperationV1{op}, nil
}

func RenderNullableColumnV1(op NullableColumnOperationV1) ([]byte, error) {
	if err := validateNullableOperation(op.value); err != nil {
		return nil, err
	}
	return []byte("ALTER TABLE ONLY \"public\".\"" + op.value.Relation + "\" ADD COLUMN \"" + op.value.Column + "\" pg_catalog.int8;\n"), nil
}

func nullableText(s string) bool {
	if len(s) == 0 || len(s) > 1024 || !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if c < 32 || c == 127 || c == utf8.RuneError {
			return false
		}
	}
	return true
}

func nullableNamespace(s string) bool { return s == "public" || s == "pg_catalog" }

func nullableObjectKey(ref nullableRef) string { return ref.Kind + ":" + ref.Identity }

func nullableTopRef(ref nullableRef, kind string) bool {
	return ref.Kind == kind && ref.Parent == "" && identifier.MatchString(ref.Identity)
}

func nullableDefinition(raw string, row bool) ([]any, error) {
	v, err := nullableJSON([]byte(raw), nullableDefinitionLimit)
	if err != nil {
		return nil, err
	}
	a, ok := v.([]any)
	if !ok || len(a) != 14 {
		return nil, refuse("nullable-definition")
	}
	for _, i := range []int{0, 1, 3, 4, 7, 8, 9, 10, 11} {
		s, ok := a[i].(string)
		if !ok || len(s) > 1024 || (s != "" && !nullableText(s)) {
			return nil, refuse("nullable-definition")
		}
	}
	for _, i := range []int{2, 6} {
		if _, ok := a[i].(bool); !ok {
			return nil, refuse("nullable-definition")
		}
	}
	length, ok := a[5].(json.Number)
	if !ok {
		return nil, refuse("nullable-definition")
	}
	n, err := strconv.ParseInt(length.String(), 10, 16)
	if err != nil || n < -32768 || n > 32767 || a[2] != false || a[4] != "" {
		return nil, refuse("nullable-definition")
	}
	fields, ok := a[12].([]any)
	if !ok || len(fields) > nullableSlotLimit {
		return nil, refuse("nullable-definition")
	}
	tail, ok := a[13].([]any)
	if !ok || len(tail) != 0 {
		return nil, refuse("nullable-definition")
	}
	if row {
		if a[0] != "c" || a[1] != "C" || len(fields) == 0 {
			return nil, refuse("nullable-definition")
		}
	} else if a[0] != "b" || a[1] != "A" || len(fields) != 0 {
		return nil, refuse("nullable-definition")
	}
	for i, field := range fields {
		f, ok := field.([]any)
		if !ok || len(f) != 5 || f[0] != json.Number(strconv.Itoa(i+1)) {
			return nil, refuse("nullable-definition")
		}
		name, nameOK := f[1].(string)
		typ, typeOK := f[2].(string)
		coll, collOK := f[4].(string)
		if _, ok := f[3].(bool); !ok || !nameOK || !identifier.MatchString(name) || !typeOK || !nullableText(typ) || !collOK || len(coll) > 128 || (coll != "" && !nullableText(coll)) {
			return nil, refuse("nullable-definition")
		}
	}
	return a, nil
}

func validateNullableObservation(v nullableObservation) error {
	s, g := v.Snapshot, v.Graph
	if v.FormatVersion != 1 || v.PostgresMajor != 17 || s.Version != Version || len(s.Objects) == 0 || s.Unknown == nil || len(s.Unknown) != 0 || len(s.unknownIdentities) != 0 || len(s.Seeds) != 0 || !s.Ownership.MigrationOwnerOwnsDatabase || !s.Ownership.MigrationOwnerOwnsPublicSchema {
		return refuse("nullable-snapshot")
	}
	if len(s.CountOnly) != len(countKeys) {
		return refuse("nullable-snapshot")
	}
	for _, k := range countKeys {
		if n, ok := s.CountOnly[k]; !ok || n < 0 {
			return refuse("nullable-snapshot")
		}
	}
	canonical, err := CanonicalCatalog(Catalog{Version: s.Version, Objects: s.Objects})
	if err != nil || digest(canonical) != s.SHA256 || !reflect.DeepEqual(s.Objects, sortedObjects(s.Objects)) {
		return refuse("nullable-snapshot")
	}
	objects := map[string]Object{}
	for _, o := range s.Objects {
		if o.Missing || o.Attributes["owner"] == "OTHER" || strings.Contains(o.Attributes["acl"], "OTHER") || strings.Contains(o.Attributes["roles"], "OTHER") {
			return refuse("nullable-snapshot")
		}
		for k, val := range o.Attributes {
			if attributes[o.Kind][k] == "int" && !nullableInteger.MatchString(val) {
				return refuse("nullable-snapshot")
			}
		}
		objects[o.Kind+":"+o.Identity] = o
	}
	for _, o := range s.Objects {
		child := o.Kind == "columns" || o.Kind == "constraints" || o.Kind == "indexes" || o.Kind == "triggers" || o.Kind == "policies"
		if child {
			if o.Parent == "" {
				return refuse("nullable-snapshot")
			}
			if _, ok := objects["relations:"+o.Parent]; !ok {
				return refuse("nullable-snapshot")
			}
		} else if o.Parent != "" {
			return refuse("nullable-snapshot")
		}
	}
	if g.FormatVersion != 1 || g.CatalogSHA256 != s.SHA256 || !nullableTopRef(g.Relation.Object, "relations") || !nullableTopRef(g.RowType.Object, "types") || !nullableTopRef(g.ArrayType.Object, "types") || g.RowType.Object == g.ArrayType.Object || g.Relation.RowType != g.RowType.Object || g.RowType.Relation != g.Relation.Object || g.RowType.Array != g.ArrayType.Object || g.ArrayType.Element != g.RowType.Object {
		return refuse("nullable-graph")
	}
	r := g.Relation
	if r.Relkind != "r" || r.Persistence != "p" || r.IsPartition || r.OfType != nil || r.Parents == nil || len(r.Parents) != 0 || r.Children == nil || len(r.Children) != 0 {
		return refuse("nullable-relation")
	}
	relation, relationOK := objects[nullableObjectKey(r.Object)]
	row, rowOK := objects[nullableObjectKey(g.RowType.Object)]
	array, arrayOK := objects[nullableObjectKey(g.ArrayType.Object)]
	if !relationOK || relation.Parent != "" || relation.Attributes["relkind"] != "r" || !rowOK || row.Parent != "" || row.Attributes["type"] != "c" || !arrayOK || array.Parent != "" || array.Attributes["type"] != "b" || row.Attributes["definition_sha256"] != digest([]byte(g.RowType.DefinitionJSON)) || array.Attributes["definition_sha256"] != digest([]byte(g.ArrayType.DefinitionJSON)) {
		return refuse("nullable-graph")
	}
	rowDefinition, err := nullableDefinition(g.RowType.DefinitionJSON, true)
	if err != nil {
		return err
	}
	if _, err := nullableDefinition(g.ArrayType.DefinitionJSON, false); err != nil {
		return err
	}
	fields := rowDefinition[12].([]any)
	if g.Slots == nil || len(g.Slots) == 0 || len(g.Slots) > nullableSlotLimit || len(g.Slots) != len(fields) {
		return refuse("nullable-slots")
	}
	seen := map[string]bool{}
	for i, slot := range g.Slots {
		if slot.Number != i+1 || !identifier.MatchString(slot.Name) || seen[slot.Name] || !nullableNamespace(slot.TypeNamespace) || !identifier.MatchString(slot.TypeName) || !nullableText(slot.TypeText) || slot.TypeModifier < -1 || slot.TypeModifier > 2147483647 || slot.Dropped || !slot.Local || slot.InheritedCount != 0 || !safeAttribute("identity", slot.Identity) || !safeAttribute("generated", slot.Generated) || !safeAttribute("acl", slot.ACL) || strings.Contains(slot.ACL, "OTHER") {
			return refuse("nullable-slots")
		}
		seen[slot.Name] = true
		collation, fieldCollation := "", ""
		if slot.CollationName != "" || slot.CollationNamespace != "" {
			if slot.CollationNamespace != "pg_catalog" || slot.CollationName != "default" {
				return refuse("nullable-slots")
			}
			collation = slot.CollationNamespace + "." + slot.CollationName
			fieldCollation = `"default"`
		}
		column, ok := objects["columns:"+r.Object.Identity+"."+slot.Name]
		a := column.Attributes
		if !ok || column.Parent != r.Object.Identity || a["position"] != strconv.Itoa(slot.Number) || a["type_sha256"] != digest([]byte(slot.TypeText)) || a["not_null"] != strconv.FormatBool(slot.NotNull) || a["identity"] != slot.Identity || a["generated"] != slot.Generated || a["acl"] != slot.ACL || a["collation_sha256"] != digest([]byte(collation)) || (a["default_sha256"] != digest(nil)) != slot.DefaultPresent {
			return refuse("nullable-slots")
		}
		field := fields[i].([]any)
		// Only the fixed pg_catalog.default regcollation spelling is supported.
		// Other collations require a separately reviewed representation.
		if field[1] != slot.Name || field[2] != slot.TypeText || field[3] != slot.NotNull || field[4] != fieldCollation {
			return refuse("nullable-slots")
		}
	}
	for _, o := range s.Objects {
		if o.Kind == "columns" && o.Parent == r.Object.Identity && !seen[strings.TrimPrefix(o.Identity, r.Object.Identity+".")] {
			return refuse("nullable-slots")
		}
	}
	sec := g.Security
	if !shaPattern.MatchString(sec.DatabaseACL) || !shaPattern.MatchString(sec.PublicSchemaACL) || sec.DefaultACLCount != 0 || sec.OtherACLEntries != 0 || sec.OtherPolicyRoles != 0 || s.CountOnly["default_acls"] != 0 || s.CountOnly["other_acl_entries"] != 0 || s.CountOnly["other_policy_roles"] != 0 {
		return refuse("nullable-security")
	}
	if _, err := nullableCanonical(g, nullableGraphLimit); err != nil {
		return err
	}
	return nil
}

func ParseNullableObservationV1(raw []byte) (NullableObservationV1, error) {
	var v nullableObservation
	if err := nullableDecode(raw, nullableObservationLimit, &v); err != nil {
		return NullableObservationV1{}, err
	}
	if err := validateNullableObservation(v); err != nil {
		return NullableObservationV1{}, err
	}
	return NullableObservationV1{v}, nil
}

func CanonicalNullableObservationV1(value NullableObservationV1) ([]byte, error) {
	if err := validateNullableObservation(value.value); err != nil {
		return nil, err
	}
	return nullableCanonical(value.value, nullableObservationLimit)
}

func ExplainNullableColumnV1(op NullableColumnOperationV1, before, after NullableObservationV1) (NullableExplanationV1, error) {
	statement, err := RenderNullableColumnV1(op)
	if err != nil {
		return NullableExplanationV1{}, err
	}
	b, err := CanonicalNullableObservationV1(before)
	if err != nil {
		return NullableExplanationV1{}, err
	}
	a, err := CanonicalNullableObservationV1(after)
	if err != nil {
		return NullableExplanationV1{}, err
	}
	bg, ag := before.value.Graph, after.value.Graph
	n := len(bg.Slots)
	if bg.Relation.Object.Identity != op.value.Relation || !reflect.DeepEqual(bg.Relation, ag.Relation) || bg.RowType.Object != ag.RowType.Object || bg.RowType.Relation != ag.RowType.Relation || bg.RowType.Array != ag.RowType.Array || bg.ArrayType != ag.ArrayType || bg.Security != ag.Security || !reflect.DeepEqual(before.value.Snapshot.CountOnly, after.value.Snapshot.CountOnly) || n < 1 || n >= nullableSlotLimit || len(ag.Slots) != n+1 || !reflect.DeepEqual(bg.Slots, ag.Slots[:n]) {
		return NullableExplanationV1{}, refuse("nullable-transition")
	}
	for _, s := range bg.Slots {
		if s.Name == op.value.Column {
			return NullableExplanationV1{}, refuse("nullable-transition")
		}
	}
	wantSlot := nullableSlot{Number: n + 1, Name: op.value.Column, TypeNamespace: "pg_catalog", TypeName: "int8", TypeModifier: -1, TypeText: "bigint", Local: true}
	if ag.Slots[n] != wantSlot {
		return NullableExplanationV1{}, refuse("nullable-transition")
	}
	bd, _ := nullableDefinition(bg.RowType.DefinitionJSON, true)
	ad, _ := nullableDefinition(ag.RowType.DefinitionJSON, true)
	for i := range bd {
		if i != 12 && !reflect.DeepEqual(bd[i], ad[i]) {
			return NullableExplanationV1{}, refuse("nullable-transition")
		}
	}
	bf, af := bd[12].([]any), ad[12].([]any)
	if !reflect.DeepEqual(bf, af[:n]) || !reflect.DeepEqual(af[n], []any{json.Number(strconv.Itoa(n + 1)), op.value.Column, "bigint", false, ""}) {
		return NullableExplanationV1{}, refuse("nullable-transition")
	}
	added := nullableRef{"columns", op.value.Relation + "." + op.value.Column, op.value.Relation}
	old := map[string]Object{}
	for _, o := range before.value.Snapshot.Objects {
		old[o.Kind+":"+o.Identity] = o
	}
	if len(after.value.Snapshot.Objects) != len(old)+1 {
		return NullableExplanationV1{}, refuse("nullable-transition")
	}
	addedSeen, rowChanged := false, false
	for _, o := range after.value.Snapshot.Objects {
		key := o.Kind + ":" + o.Identity
		if key == nullableObjectKey(added) {
			if _, exists := old[key]; exists || o.Parent != added.Parent {
				return NullableExplanationV1{}, refuse("nullable-transition")
			}
			want := map[string]string{"position": strconv.Itoa(n + 1), "type_sha256": digest([]byte("bigint")), "not_null": "false", "default_sha256": digest(nil), "identity": "", "generated": "", "collation_sha256": digest(nil), "acl": ""}
			if !reflect.DeepEqual(o.Attributes, want) {
				return NullableExplanationV1{}, refuse("nullable-transition")
			}
			addedSeen = true
			continue
		}
		prior, ok := old[key]
		if !ok || prior.Kind != o.Kind || prior.Identity != o.Identity || prior.Parent != o.Parent || prior.Missing != o.Missing {
			return NullableExplanationV1{}, refuse("nullable-transition")
		}
		for k, v := range prior.Attributes {
			if key == nullableObjectKey(bg.RowType.Object) && k == "definition_sha256" {
				rowChanged = v != o.Attributes[k]
				continue
			}
			if v != o.Attributes[k] {
				return NullableExplanationV1{}, refuse("nullable-transition")
			}
		}
		delete(old, key)
	}
	if !addedSeen || !rowChanged || len(old) != 0 {
		return NullableExplanationV1{}, refuse("nullable-transition")
	}
	operation, _ := nullableCanonical(op.value, nullableOperationLimit)
	return NullableExplanationV1{nullableExplanation{1, digest(operation), digest(statement), before.value.Snapshot.SHA256, after.value.Snapshot.SHA256, digest(b), digest(a), []nullableRef{added, bg.RowType.Object}}}, nil
}

func CanonicalNullableExplanationV1(value NullableExplanationV1) ([]byte, error) {
	v := value.value
	if v.FormatVersion != 1 || len(v.Explained) != 2 || !nullableTopRef(v.Explained[1], "types") || v.Explained[0].Kind != "columns" || !safeIdentity(Object{Kind: v.Explained[0].Kind, Identity: v.Explained[0].Identity, Parent: v.Explained[0].Parent}) || v.Explained[0].Parent == "" {
		return nil, refuse("nullable-explanation")
	}
	for _, h := range []string{v.OperationSHA256, v.StatementSHA256, v.BeforeCatalogSHA256, v.AfterCatalogSHA256, v.BeforeObservationSHA256, v.AfterObservationSHA256} {
		if !shaPattern.MatchString(h) {
			return nil, refuse("nullable-explanation")
		}
	}
	return nullableCanonical(v, nullableOperationLimit)
}

type nullableGraphCapture struct {
	operation   nullableOperation
	observation NullableObservationV1
}

func CaptureNullableGraphV1(ctx context.Context, runtimeDSN string, opt Options, op NullableColumnOperationV1) (NullableObservationV1, error) {
	if err := validateNullableOperation(op.value); err != nil {
		return NullableObservationV1{}, err
	}
	if opt.IncludeSeeds {
		return NullableObservationV1{}, refuse("nullable-capture")
	}
	request := &nullableGraphCapture{operation: op.value}
	if _, err := captureWithNullableGraph(ctx, runtimeDSN, opt, request); err != nil {
		return NullableObservationV1{}, err
	}
	return request.observation, nil
}

const nullableRelationSQL = `SELECT c.relname::text,t.typname::text,a.typname::text,
c.relkind::text,c.relpersistence::text,c.relispartition,c.reloftype::bigint,
(SELECT count(*) FROM pg_catalog.pg_inherits WHERE inhrelid=c.oid),
(SELECT count(*) FROM pg_catalog.pg_inherits WHERE inhparent=c.oid),
(tn.nspname='public' AND an.nspname='public' AND t.typtype='c' AND a.typtype='b'
 AND t.typrelid=c.oid AND a.typelem=t.oid
 AND (SELECT count(*) FROM pg_catalog.pg_type WHERE typrelid=c.oid)=1
 AND (SELECT count(*) FROM pg_catalog.pg_type WHERE typelem=t.oid)=1)
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
JOIN pg_catalog.pg_type t ON t.oid=c.reltype JOIN pg_catalog.pg_namespace tn ON tn.oid=t.typnamespace
JOIN pg_catalog.pg_type a ON a.oid=t.typarray JOIN pg_catalog.pg_namespace an ON an.oid=a.typnamespace
WHERE n.nspname='public' AND c.relname=$1`

var nullableSlotsSQL = `SELECT a.attnum::integer,a.attname::text,tn.nspname::text,t.typname::text,
a.atttypmod,pg_catalog.format_type(a.atttypid,a.atttypmod),a.attnotnull,a.attisdropped,
a.attislocal,a.attinhcount,EXISTS(SELECT 1 FROM pg_catalog.pg_attrdef WHERE adrelid=c.oid AND adnum=a.attnum),
a.attidentity::text,a.attgenerated::text,COALESCE(cn.nspname,''),COALESCE(co.collname,''),
(` + aclExpr("COALESCE(a.attacl,'{}'::pg_catalog.aclitem[])") + `)::text
FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid
JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
LEFT JOIN pg_catalog.pg_type t ON t.oid=a.atttypid LEFT JOIN pg_catalog.pg_namespace tn ON tn.oid=t.typnamespace
LEFT JOIN pg_catalog.pg_collation co ON co.oid=a.attcollation LEFT JOIN pg_catalog.pg_namespace cn ON cn.oid=co.collnamespace
WHERE n.nspname='public' AND c.relname=$1 AND a.attnum>0 ORDER BY a.attnum LIMIT 1025`

var nullableSecuritySQL = `SELECT 'database'::text,d.datdba::text,(` + aclExpr("COALESCE(d.datacl,pg_catalog.acldefault('d',d.datdba))") + `)::text
FROM pg_catalog.pg_database d WHERE d.datname=pg_catalog.current_database()
UNION ALL SELECT 'public'::text,n.nspowner::text,(` + aclExpr("COALESCE(n.nspacl,pg_catalog.acldefault('n',n.nspowner))") + `)::text
FROM pg_catalog.pg_namespace n WHERE n.nspname='public'`

func nullableACL(raw string, roles symbols) (string, error) {
	if _, err := nullableJSON([]byte(raw), maxAttributeBytes); err != nil {
		return "", err
	}
	var values []struct {
		Grantee   string `json:"grantee"`
		Grantor   string `json:"grantor"`
		Privilege string `json:"privilege"`
		Grantable bool   `json:"grantable"`
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&values) != nil || values == nil || len(values) > 1024 {
		return "", refuse("nullable-security")
	}
	entries := map[string]bool{}
	for _, v := range values {
		grantor, grantee := roles.role(v.Grantor), roles.role(v.Grantee)
		if grantor == "OTHER" || grantee == "OTHER" {
			return "", refuse("nullable-security")
		}
		entry := grantor + ">" + grantee + ":" + v.Privilege + ":" + strconv.FormatBool(v.Grantable)
		if entries[entry] {
			return "", refuse("nullable-security")
		}
		entries[entry] = true
	}
	list := make([]string, 0, len(entries))
	for entry := range entries {
		list = append(list, entry)
	}
	sort.Strings(list)
	joined := strings.Join(list, ";")
	if !safeAttribute("acl", joined) {
		return "", refuse("nullable-security")
	}
	return joined, nil
}

func captureNullableGraphInTx(ctx context.Context, tx pgx.Tx, roles symbols, snapshot Snapshot, op nullableOperation, remaining int) (NullableObservationV1, error) {
	charge := func(n int) error {
		if n < 0 || n > remaining {
			return refuse("nullable-limit")
		}
		remaining -= n
		return nil
	}
	var relation, row, array, kind, persistence string
	var partition, reciprocal bool
	var typed, parents, children int64
	if tx.QueryRow(ctx, nullableRelationSQL, op.Relation).Scan(&relation, &row, &array, &kind, &persistence, &partition, &typed, &parents, &children, &reciprocal) != nil {
		return NullableObservationV1{}, refuse("nullable-read")
	}
	if err := charge(len(relation) + len(row) + len(array) + len(kind) + len(persistence) + 64); err != nil {
		return NullableObservationV1{}, err
	}
	if relation != op.Relation || !identifier.MatchString(row) || !identifier.MatchString(array) || row == array || kind != "r" || persistence != "p" || partition || typed != 0 || parents != 0 || children != 0 || !reciprocal {
		return NullableObservationV1{}, refuse("nullable-relation")
	}
	rref, tref, aref := nullableRef{"relations", relation, ""}, nullableRef{"types", row, ""}, nullableRef{"types", array, ""}
	g := nullableGraph{FormatVersion: 1, CatalogSHA256: snapshot.SHA256, Relation: nullableRelation{rref, kind, persistence, false, nil, []nullableRef{}, []nullableRef{}, tref}, RowType: nullableRowType{Object: tref, Relation: rref, Array: aref}, ArrayType: nullableArrayType{Object: aref, Element: tref}, Slots: []nullableSlot{}}
	// The expression hashed by normal capture has one authority: queries/types.
	typeSQL := ""
	for _, q := range queries {
		if q.kind == "types" {
			if typeSQL != "" {
				return NullableObservationV1{}, refuse("nullable-read")
			}
			typeSQL = q.sql
		}
	}
	if typeSQL == "" {
		return NullableObservationV1{}, refuse("nullable-read")
	}
	types, err := tx.Query(ctx, "SELECT name,parent,payload FROM ("+typeSQL+") AS graph_type(name,parent,payload) WHERE name=$1 OR name=$2 ORDER BY name LIMIT 3", row, array)
	if err != nil {
		return NullableObservationV1{}, refuse("nullable-read")
	}
	definitions := map[string]string{}
	for types.Next() {
		var name, parent, payload string
		if types.Scan(&name, &parent, &payload) != nil || len(payload) > maxAttributeBytes {
			types.Close()
			return NullableObservationV1{}, refuse("nullable-read")
		}
		if err := charge(len(name) + len(parent) + len(payload)); err != nil {
			types.Close()
			return NullableObservationV1{}, err
		}
		if parent != "" || (name != row && name != array) || len(definitions) >= 2 {
			types.Close()
			return NullableObservationV1{}, refuse("nullable-read")
		}
		if _, exists := definitions[name]; exists {
			types.Close()
			return NullableObservationV1{}, refuse("nullable-read")
		}
		var fields map[string]json.RawMessage
		var definition string
		if json.Unmarshal([]byte(payload), &fields) != nil || json.Unmarshal(fields["definition"], &definition) != nil || len(definition) > nullableDefinitionLimit {
			types.Close()
			return NullableObservationV1{}, refuse("nullable-read")
		}
		definitions[name] = definition
	}
	err = types.Err()
	types.Close()
	if err != nil || len(definitions) != 2 {
		return NullableObservationV1{}, refuse("nullable-read")
	}
	g.RowType.DefinitionJSON, g.ArrayType.DefinitionJSON = definitions[row], definitions[array]
	rows, err := tx.Query(ctx, nullableSlotsSQL, relation)
	if err != nil {
		return NullableObservationV1{}, refuse("nullable-read")
	}
	for rows.Next() {
		if len(g.Slots) >= nullableSlotLimit {
			rows.Close()
			return NullableObservationV1{}, refuse("nullable-limit")
		}
		var slot nullableSlot
		var acl string
		if rows.Scan(&slot.Number, &slot.Name, &slot.TypeNamespace, &slot.TypeName, &slot.TypeModifier, &slot.TypeText, &slot.NotNull, &slot.Dropped, &slot.Local, &slot.InheritedCount, &slot.DefaultPresent, &slot.Identity, &slot.Generated, &slot.CollationNamespace, &slot.CollationName, &acl) != nil || len(acl) > maxAttributeBytes {
			rows.Close()
			return NullableObservationV1{}, refuse("nullable-read")
		}
		// Charge complete retained row and original ACL payload before append.
		wire, e := nullableCanonical(slot, nullableGraphLimit)
		if e != nil {
			rows.Close()
			return NullableObservationV1{}, e
		}
		if e = charge(len(wire) + len(acl)); e != nil {
			rows.Close()
			return NullableObservationV1{}, e
		}
		slot.ACL, e = nullableACL(acl, roles)
		if e != nil {
			rows.Close()
			return NullableObservationV1{}, e
		}
		g.Slots = append(g.Slots, slot)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return NullableObservationV1{}, refuse("nullable-read")
	}
	security, err := tx.Query(ctx, nullableSecuritySQL)
	if err != nil {
		return NullableObservationV1{}, refuse("nullable-read")
	}
	hashes := map[string]string{}
	for security.Next() {
		var surface, owner, raw string
		if security.Scan(&surface, &owner, &raw) != nil || len(raw) > maxAttributeBytes {
			security.Close()
			return NullableObservationV1{}, refuse("nullable-read")
		}
		if e := charge(len(surface) + len(owner) + len(raw)); e != nil {
			security.Close()
			return NullableObservationV1{}, e
		}
		if (surface != "database" && surface != "public") || hashes[surface] != "" || roles.role(owner) != "MIGRATION_OWNER" {
			security.Close()
			return NullableObservationV1{}, refuse("nullable-security")
		}
		acl, e := nullableACL(raw, roles)
		if e != nil {
			security.Close()
			return NullableObservationV1{}, e
		}
		wire, e := nullableCanonical(map[string]string{"owner": "MIGRATION_OWNER", "acl": acl}, nullableGraphLimit)
		if e != nil {
			security.Close()
			return NullableObservationV1{}, e
		}
		hashes[surface] = digest(wire)
	}
	err = security.Err()
	security.Close()
	if err != nil || len(hashes) != 2 {
		return NullableObservationV1{}, refuse("nullable-read")
	}
	g.Security = nullableSecurity{hashes["database"], hashes["public"], snapshot.CountOnly["default_acls"], snapshot.CountOnly["other_acl_entries"], snapshot.CountOnly["other_policy_roles"]}
	graphWire, err := nullableCanonical(g, nullableGraphLimit)
	if err != nil {
		return NullableObservationV1{}, err
	}
	if err := charge(len(graphWire)); err != nil {
		return NullableObservationV1{}, err
	}
	v := NullableObservationV1{nullableObservation{1, 17, snapshot, g}}
	if _, err := CanonicalNullableObservationV1(v); err != nil {
		return NullableObservationV1{}, err
	}
	return v, nil
}
