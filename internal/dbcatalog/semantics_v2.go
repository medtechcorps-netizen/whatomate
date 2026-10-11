package dbcatalog

// V2 retains the complete default-ACL graph. Parsing is not qualification:
// explanation also requires the fixed source contract at both endpoints.
import (
	"context"
	"reflect"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
)

const nullableDefaultRowsLimit = 32
const nullableDefaultItemsLimit = 256
const nullableDefaultTuplesLimit = 1024
const nullableDefaultContractSHA256 = "6f51817c3516988a08b66193fedbb00a375bf8a23459c297593af3f675b37c53"
const nullableDefaultContract = `{"default_acl_rows":[{"acl":[{"grantable":false,"grantee":"RUNTIME","grantor":"MIGRATION_OWNER","privilege":"SELECT"},{"grantable":false,"grantee":"RUNTIME","grantor":"MIGRATION_OWNER","privilege":"USAGE"}],"namespace":"public","object_type":"S","owner":"MIGRATION_OWNER"},{"acl":[{"grantable":false,"grantee":"RUNTIME","grantor":"MIGRATION_OWNER","privilege":"DELETE"},{"grantable":false,"grantee":"RUNTIME","grantor":"MIGRATION_OWNER","privilege":"INSERT"},{"grantable":false,"grantee":"RUNTIME","grantor":"MIGRATION_OWNER","privilege":"SELECT"},{"grantable":false,"grantee":"RUNTIME","grantor":"MIGRATION_OWNER","privilege":"UPDATE"}],"namespace":"public","object_type":"r","owner":"MIGRATION_OWNER"}],"format_version":1,"postgres_major":17,"profile":"tenant-rls-v5-pg17-default-acl-v1"}` + "\n"

type NullableObservationV2 struct{ value nullableObservationV2 }
type NullableExplanationV2 struct{ value nullableExplanationV2 }
type nullableDefaultGrant struct {
	Grantor   string `json:"grantor"`
	Grantee   string `json:"grantee"`
	Privilege string `json:"privilege"`
	Grantable bool   `json:"grantable"`
}
type nullableDefaultRow struct {
	Owner      string                 `json:"owner"`
	Namespace  *string                `json:"namespace"`
	ObjectType string                 `json:"object_type"`
	ACL        []nullableDefaultGrant `json:"acl"`
}
type nullableSecurityV2 struct {
	DatabaseACL      string               `json:"database_acl_sha256"`
	PublicSchemaACL  string               `json:"public_schema_acl_sha256"`
	DefaultACLCount  int64                `json:"default_acl_count"`
	OtherACLEntries  int64                `json:"other_acl_entries"`
	OtherPolicyRoles int64                `json:"other_policy_roles"`
	DefaultACLRows   []nullableDefaultRow `json:"default_acl_rows"`
}
type nullableGraphV2 struct {
	FormatVersion int                `json:"format_version"`
	CatalogSHA256 string             `json:"catalog_sha256"`
	Relation      nullableRelation   `json:"relation"`
	RowType       nullableRowType    `json:"row_type"`
	ArrayType     nullableArrayType  `json:"array_type"`
	Slots         []nullableSlot     `json:"slots"`
	Security      nullableSecurityV2 `json:"security"`
}
type nullableObservationV2 struct {
	FormatVersion int             `json:"format_version"`
	PostgresMajor int             `json:"postgres_major"`
	Snapshot      Snapshot        `json:"snapshot"`
	Graph         nullableGraphV2 `json:"graph"`
}
type nullableExplanationV2 struct {
	FormatVersion            int           `json:"format_version"`
	OperationSHA256          string        `json:"operation_sha256"`
	StatementSHA256          string        `json:"statement_sha256"`
	BeforeCatalogSHA256      string        `json:"before_catalog_sha256"`
	AfterCatalogSHA256       string        `json:"after_catalog_sha256"`
	BeforeObservationSHA256  string        `json:"before_observation_sha256"`
	AfterObservationSHA256   string        `json:"after_observation_sha256"`
	Explained                []nullableRef `json:"explained"`
	DefaultACLContractSHA256 string        `json:"default_acl_contract_sha256"`
}

// This is shared unversioned data, not a fabricated v1 observation. Counters,
// snapshot, graph version and all security hashes retain their actual values.
func (g nullableGraphV2) parts() nullableGraph {
	s := g.Security
	return nullableGraph{g.FormatVersion, g.CatalogSHA256, g.Relation, g.RowType, g.ArrayType, g.Slots, nullableSecurity{s.DatabaseACL, s.PublicSchemaACL, s.DefaultACLCount, s.OtherACLEntries, s.OtherPolicyRoles}}
}
func nullableDefaultOwner(s string) bool { return s == "MIGRATION_OWNER" || s == "RUNTIME" }
func nullableDefaultPrivilege(kind, p string) bool {
	switch kind {
	case "r":
		switch p {
		case "DELETE", "INSERT", "MAINTAIN", "REFERENCES", "SELECT", "TRIGGER", "TRUNCATE", "UPDATE":
			return true
		}
	case "S":
		return p == "SELECT" || p == "UPDATE" || p == "USAGE"
	case "f":
		return p == "EXECUTE"
	case "T":
		return p == "USAGE"
	case "n":
		return p == "CREATE" || p == "USAGE"
	}
	return false
}
func nullableDefaultRowKey(r nullableDefaultRow) string {
	ns := ""
	if r.Namespace != nil {
		ns = *r.Namespace
	}
	return r.Owner + ":" + ns + ":" + r.ObjectType
}
func nullableDefaultGrantKey(g nullableDefaultGrant) string {
	return g.Grantor + ":" + g.Grantee + ":" + g.Privilege
}
func validateNullableDefaultRows(rows []nullableDefaultRow) error {
	if rows == nil || len(rows) > nullableDefaultRowsLimit {
		return refuse("nullable-default-acl")
	}
	previous := ""
	total := 0
	for _, r := range rows {
		key := nullableDefaultRowKey(r)
		if !nullableDefaultOwner(r.Owner) || (r.Namespace != nil && *r.Namespace != "public") || (r.ObjectType != "r" && r.ObjectType != "S" && r.ObjectType != "f" && r.ObjectType != "T" && r.ObjectType != "n") || (r.ObjectType == "n" && r.Namespace != nil) || key <= previous || r.ACL == nil {
			return refuse("nullable-default-acl")
		}
		previous = key
		prevGrant := ""
		pairs := map[string]bool{}
		for _, g := range r.ACL {
			total++
			k := nullableDefaultGrantKey(g)
			if total > nullableDefaultTuplesLimit || !nullableDefaultOwner(g.Grantor) || (!nullableDefaultOwner(g.Grantee) && g.Grantee != "PUBLIC") || !nullableDefaultPrivilege(r.ObjectType, g.Privilege) || k <= prevGrant {
				return refuse("nullable-default-acl")
			}
			prevGrant = k
			pairs[g.Grantor+":"+g.Grantee] = true
		}
		if len(pairs) > nullableDefaultItemsLimit {
			return refuse("nullable-default-acl")
		}
	}
	return nil
}
func nullableExpectedDefaultRows() ([]nullableDefaultRow, error) {
	var c struct {
		FormatVersion int                  `json:"format_version"`
		Profile       string               `json:"profile"`
		PostgresMajor int                  `json:"postgres_major"`
		Rows          []nullableDefaultRow `json:"default_acl_rows"`
	}
	if digest([]byte(nullableDefaultContract)) != nullableDefaultContractSHA256 || nullableDecode([]byte(nullableDefaultContract), nullableGraphLimit, &c) != nil || c.FormatVersion != 1 || c.PostgresMajor != 17 || c.Profile != "tenant-rls-v5-pg17-default-acl-v1" || validateNullableDefaultRows(c.Rows) != nil {
		return nil, refuse("nullable-default-contract")
	}
	return c.Rows, nil
}
func validateNullableObservationV2(v nullableObservationV2) error {
	if v.FormatVersion != 2 || v.PostgresMajor != 17 {
		return refuse("nullable-snapshot")
	}
	if err := validateNullableDefaultRows(v.Graph.Security.DefaultACLRows); err != nil {
		return err
	}
	if err := validateNullableParts(v.Snapshot, v.Graph.parts(), 2, int64(len(v.Graph.Security.DefaultACLRows))); err != nil {
		return err
	}
	_, err := nullableCanonical(v.Graph, nullableGraphLimit)
	return err
}
func ParseNullableObservationV2(raw []byte) (NullableObservationV2, error) {
	var v nullableObservationV2
	if err := nullableDecode(raw, nullableObservationLimit, &v); err != nil {
		return NullableObservationV2{}, err
	}
	if err := validateNullableObservationV2(v); err != nil {
		return NullableObservationV2{}, err
	}
	return NullableObservationV2{v}, nil
}
func CanonicalNullableObservationV2(v NullableObservationV2) ([]byte, error) {
	if err := validateNullableObservationV2(v.value); err != nil {
		return nil, err
	}
	return nullableCanonical(v.value, nullableObservationLimit)
}
func ExplainNullableColumnV2(op NullableColumnOperationV1, before, after NullableObservationV2) (NullableExplanationV2, error) {
	statement, err := RenderNullableColumnV1(op)
	if err != nil {
		return NullableExplanationV2{}, err
	}
	b, err := CanonicalNullableObservationV2(before)
	if err != nil {
		return NullableExplanationV2{}, err
	}
	a, err := CanonicalNullableObservationV2(after)
	if err != nil {
		return NullableExplanationV2{}, err
	}
	expected, err := nullableExpectedDefaultRows()
	if err != nil {
		return NullableExplanationV2{}, err
	}
	if !reflect.DeepEqual(before.value.Graph.Security.DefaultACLRows, expected) || !reflect.DeepEqual(after.value.Graph.Security.DefaultACLRows, expected) {
		return NullableExplanationV2{}, refuse("nullable-default-contract")
	}
	explained, err := explainNullableParts(op, before.value.Snapshot, after.value.Snapshot, before.value.Graph.parts(), after.value.Graph.parts())
	if err != nil {
		return NullableExplanationV2{}, err
	}
	operation, _ := nullableCanonical(op.value, nullableOperationLimit)
	return NullableExplanationV2{nullableExplanationV2{2, digest(operation), digest(statement), before.value.Snapshot.SHA256, after.value.Snapshot.SHA256, digest(b), digest(a), explained, nullableDefaultContractSHA256}}, nil
}
func CanonicalNullableExplanationV2(value NullableExplanationV2) ([]byte, error) {
	v := value.value
	if v.FormatVersion != 2 || v.DefaultACLContractSHA256 != nullableDefaultContractSHA256 || len(v.Explained) != 2 || !nullableTopRef(v.Explained[1], "types") || v.Explained[0].Kind != "columns" || !safeIdentity(Object{Kind: v.Explained[0].Kind, Identity: v.Explained[0].Identity, Parent: v.Explained[0].Parent}) || v.Explained[0].Parent == "" {
		return nil, refuse("nullable-explanation")
	}
	for _, h := range []string{v.OperationSHA256, v.StatementSHA256, v.BeforeCatalogSHA256, v.AfterCatalogSHA256, v.BeforeObservationSHA256, v.AfterObservationSHA256} {
		if !shaPattern.MatchString(h) {
			return nil, refuse("nullable-explanation")
		}
	}
	return nullableCanonical(v, nullableOperationLimit)
}
func CaptureNullableGraphV2(ctx context.Context, dsn string, opt Options, op NullableColumnOperationV1) (NullableObservationV2, error) {
	if err := validateNullableOperation(op.value); err != nil {
		return NullableObservationV2{}, err
	}
	if opt.IncludeSeeds {
		return NullableObservationV2{}, refuse("nullable-capture")
	}
	request := &nullableGraphCapture{version: 2, operation: op.value}
	if _, err := captureWithNullableGraph(ctx, dsn, opt, request); err != nil {
		return NullableObservationV2{}, err
	}
	return request.observationV2, nil
}

// Complete row inventory, with a refusing overflow sentinel. CASE guards the
// functions themselves; LEFT JOIN preserves empty arrays and malformed items.
const nullableDefaultsSQL = `WITH defaults AS (
 SELECT * FROM pg_catalog.pg_default_acl ORDER BY oid LIMIT 33
)
SELECT d.oid::bigint,d.defaclrole::bigint,d.defaclnamespace::bigint,n.nspname::text,
 d.defaclobjtype::text,d.defaclacl IS NULL,pg_catalog.cardinality(d.defaclacl)::bigint,
 pg_catalog.array_ndims(d.defaclacl)::bigint,pg_catalog.array_lower(d.defaclacl,1)::bigint,
 pg_catalog.array_upper(d.defaclacl,1)::bigint,i.ordinality::bigint,i.item IS NULL,
 x.grantor::bigint,x.grantee::bigint,x.privilege_type::text,x.is_grantable
FROM defaults d LEFT JOIN pg_catalog.pg_namespace n ON n.oid=d.defaclnamespace
LEFT JOIN LATERAL pg_catalog.unnest(CASE
 WHEN d.defaclacl IS NOT NULL AND pg_catalog.cardinality(d.defaclacl) BETWEEN 1 AND 256
 AND pg_catalog.array_ndims(d.defaclacl)=1 AND pg_catalog.array_lower(d.defaclacl,1)=1
 AND pg_catalog.array_upper(d.defaclacl,1)=pg_catalog.cardinality(d.defaclacl)
 THEN d.defaclacl ELSE '{}'::pg_catalog.aclitem[] END) WITH ORDINALITY AS i(item,ordinality) ON true
LEFT JOIN LATERAL pg_catalog.aclexplode(CASE WHEN i.item IS NOT NULL
 THEN ARRAY[i.item]::pg_catalog.aclitem[] ELSE '{}'::pg_catalog.aclitem[] END) x ON true
ORDER BY d.oid,i.ordinality,x.grantor,x.grantee,x.privilege_type`

type nullableDefaultMetadata struct {
	OID, Owner, Namespace                 int64
	NamespaceName                         *string
	Kind                                  string
	Null                                  bool
	Cardinality, Dimensions, Lower, Upper *int64
}

func nullableOID(n int64, zero bool) bool { return n >= 0 && n <= 4294967295 && (zero || n > 0) }
func captureNullableDefaults(ctx context.Context, tx pgx.Tx, roles symbols, remaining int) ([]nullableDefaultRow, int, error) {
	rows, err := tx.Query(ctx, nullableDefaultsSQL)
	if err != nil {
		return nil, remaining, refuse("nullable-read")
	}
	defer rows.Close()
	result := []nullableDefaultRow{}
	total := 0
	var prior nullableDefaultMetadata
	var ordinals map[int64]string
	var pairs map[string]int64
	var tuples map[string]bool
	emptySeen := false
	finish := func() error {
		if len(result) > 0 && (prior.Cardinality == nil || int64(len(ordinals)) != *prior.Cardinality) {
			return refuse("nullable-default-acl")
		}
		return nil
	}
	for rows.Next() {
		var m nullableDefaultMetadata
		var ordinal, grantor, grantee *int64
		var nullItem bool
		var privilege *string
		var grantable *bool
		if rows.Scan(&m.OID, &m.Owner, &m.Namespace, &m.NamespaceName, &m.Kind, &m.Null, &m.Cardinality, &m.Dimensions, &m.Lower, &m.Upper, &ordinal, &nullItem, &grantor, &grantee, &privilege, &grantable) != nil {
			return nil, remaining, refuse("nullable-read")
		}
		cost := 128 + len(m.Kind)
		if m.NamespaceName != nil {
			cost += len(*m.NamespaceName)
		}
		if privilege != nil {
			cost += len(*privilege)
		}
		if cost > remaining {
			return nil, remaining, refuse("nullable-limit")
		}
		remaining -= cost
		if !nullableOID(m.OID, false) || !nullableOID(m.Owner, false) || !nullableOID(m.Namespace, true) || m.Null || m.Cardinality == nil || *m.Cardinality < 0 || *m.Cardinality > nullableDefaultItemsLimit {
			return nil, remaining, refuse("nullable-default-acl")
		}
		if *m.Cardinality == 0 {
			if m.Dimensions != nil || m.Lower != nil || m.Upper != nil {
				return nil, remaining, refuse("nullable-default-acl")
			}
		} else if m.Dimensions == nil || *m.Dimensions != 1 || m.Lower == nil || *m.Lower != 1 || m.Upper == nil || *m.Upper != *m.Cardinality {
			return nil, remaining, refuse("nullable-default-acl")
		}
		if len(result) == 0 || m.OID != prior.OID {
			if err := finish(); err != nil {
				return nil, remaining, err
			}
			if len(result) >= nullableDefaultRowsLimit || (len(result) > 0 && m.OID <= prior.OID) {
				return nil, remaining, refuse("nullable-default-acl")
			}
			owner := roles.role(strconv.FormatInt(m.Owner, 10))
			if !nullableDefaultOwner(owner) {
				return nil, remaining, refuse("nullable-default-acl")
			}
			var ns *string
			if m.Namespace == 0 {
				if m.NamespaceName != nil {
					return nil, remaining, refuse("nullable-default-acl")
				}
			} else {
				if m.NamespaceName == nil || *m.NamespaceName != "public" {
					return nil, remaining, refuse("nullable-default-acl")
				}
				v := "public"
				ns = &v
			}
			result = append(result, nullableDefaultRow{owner, ns, m.Kind, []nullableDefaultGrant{}})
			prior = m
			ordinals = map[int64]string{}
			pairs = map[string]int64{}
			tuples = map[string]bool{}
			emptySeen = false
		} else if !reflect.DeepEqual(m, prior) {
			return nil, remaining, refuse("nullable-default-acl")
		}
		if *m.Cardinality == 0 {
			if emptySeen || ordinal != nil || !nullItem || grantor != nil || grantee != nil || privilege != nil || grantable != nil {
				return nil, remaining, refuse("nullable-default-acl")
			}
			emptySeen = true
			continue
		}
		if ordinal == nil || *ordinal < 1 || *ordinal > *m.Cardinality || nullItem || grantor == nil || grantee == nil || privilege == nil || grantable == nil || !nullableOID(*grantor, false) || !nullableOID(*grantee, true) {
			return nil, remaining, refuse("nullable-default-acl")
		}
		gr := nullableDefaultGrant{roles.role(strconv.FormatInt(*grantor, 10)), roles.role(strconv.FormatInt(*grantee, 10)), *privilege, *grantable}
		pair := gr.Grantor + ":" + gr.Grantee
		key := nullableDefaultGrantKey(gr)
		if priorPair, ok := ordinals[*ordinal]; ok {
			if priorPair != pair {
				return nil, remaining, refuse("nullable-default-acl")
			}
		} else {
			if *ordinal != int64(len(ordinals)+1) {
				return nil, remaining, refuse("nullable-default-acl")
			}
			ordinals[*ordinal] = pair
		}
		if old, ok := pairs[pair]; ok && old != *ordinal {
			return nil, remaining, refuse("nullable-default-acl")
		}
		pairs[pair] = *ordinal
		if tuples[key] || !nullableDefaultOwner(gr.Grantor) || (!nullableDefaultOwner(gr.Grantee) && gr.Grantee != "PUBLIC") || !nullableDefaultPrivilege(m.Kind, gr.Privilege) {
			return nil, remaining, refuse("nullable-default-acl")
		}
		tuples[key] = true
		total++
		if total > nullableDefaultTuplesLimit {
			return nil, remaining, refuse("nullable-limit")
		}
		result[len(result)-1].ACL = append(result[len(result)-1].ACL, gr)
	}
	if rows.Err() != nil {
		return nil, remaining, refuse("nullable-read")
	}
	if err := finish(); err != nil {
		return nil, remaining, err
	}
	for i := range result {
		sort.Slice(result[i].ACL, func(a, b int) bool {
			return nullableDefaultGrantKey(result[i].ACL[a]) < nullableDefaultGrantKey(result[i].ACL[b])
		})
	}
	sort.Slice(result, func(i, j int) bool { return nullableDefaultRowKey(result[i]) < nullableDefaultRowKey(result[j]) })
	if err := validateNullableDefaultRows(result); err != nil {
		return nil, remaining, err
	}
	return result, remaining, nil
}
func captureNullableGraphV2InTx(ctx context.Context, tx pgx.Tx, roles symbols, snapshot Snapshot, op nullableOperation, remaining int) (NullableObservationV2, error) {
	g, remaining, err := captureNullablePartsInTx(ctx, tx, roles, snapshot, op, remaining, 2)
	if err != nil {
		return NullableObservationV2{}, err
	}
	rows, remaining, err := captureNullableDefaults(ctx, tx, roles, remaining)
	if err != nil {
		return NullableObservationV2{}, err
	}
	s := g.Security
	graph := nullableGraphV2{2, g.CatalogSHA256, g.Relation, g.RowType, g.ArrayType, g.Slots, nullableSecurityV2{s.DatabaseACL, s.PublicSchemaACL, s.DefaultACLCount, s.OtherACLEntries, s.OtherPolicyRoles, rows}}
	wire, err := nullableCanonical(graph, nullableGraphLimit)
	if err != nil {
		return NullableObservationV2{}, err
	}
	if len(wire) > remaining {
		return NullableObservationV2{}, refuse("nullable-limit")
	}
	v := NullableObservationV2{nullableObservationV2{2, 17, snapshot, graph}}
	if _, err := CanonicalNullableObservationV2(v); err != nil {
		return NullableObservationV2{}, err
	}
	return v, nil
}
