package dbcatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func nullableV2Document(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "nullable-column-v2.json"))
	if err != nil {
		t.Fatal("actual application PG17 fixture required; no synthetic replacement")
	}
	var d map[string]json.RawMessage
	if nullableDecode(raw, (8<<20)+(4<<10), &d) != nil || len(d) != 5 || string(d["format_version"]) != "2" {
		t.Fatal("invalid actual v2 fixture")
	}
	for _, k := range []string{"operation", "before", "after", "explanation"} {
		if d[k] == nil {
			t.Fatal("missing fixture field")
		}
	}
	return d
}
func nullableV2Fixture(t *testing.T) (NullableColumnOperationV1, NullableObservationV2, NullableObservationV2) {
	t.Helper()
	d := nullableV2Document(t)
	op, err := ParseNullableColumnOperationV1(append(d["operation"], '\n'))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseNullableObservationV2(append(d["before"], '\n'))
	if err != nil {
		t.Fatal(err)
	}
	a, err := ParseNullableObservationV2(append(d["after"], '\n'))
	if err != nil {
		t.Fatal(err)
	}
	return op, b, a
}
func nullableV2Clone(t *testing.T, v NullableObservationV2) NullableObservationV2 {
	t.Helper()
	var n nullableObservationV2
	if json.Unmarshal(nullableTestBytes(t, v.value), &n) != nil {
		t.Fatal("clone failed")
	}
	return NullableObservationV2{n}
}
func TestNullableV2ActualFixtureAndV1Separation(t *testing.T) {
	op, b, a := nullableV2Fixture(t)
	if len(b.value.Snapshot.Objects) < 1000 || len(b.value.Graph.Security.DefaultACLRows) != 2 || b.value.Snapshot.CountOnly["default_acls"] != 2 {
		t.Fatal("full application inventory/defaults absent")
	}
	e, err := ExplainNullableColumnV2(op, b, a)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := CanonicalNullableExplanationV2(e)
	if err != nil || !bytes.Equal(wire, append(nullableV2Document(t)["explanation"], '\n')) {
		t.Fatal("actual explanation changed")
	}
	for _, v := range []NullableObservationV2{b, a} {
		raw, err := CanonicalNullableObservationV2(v)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseNullableObservationV2(raw)
		if err != nil {
			t.Fatal(err)
		}
		again, err := CanonicalNullableObservationV2(parsed)
		if err != nil || !bytes.Equal(raw, again) {
			t.Fatal("v2 roundtrip drift")
		}
		if _, err := ParseNullableObservationV1(raw); err == nil {
			t.Fatal("v1 accepted v2")
		}
	}
	old, _ := nullableTestFixture(t)
	oldRaw, err := CanonicalNullableObservationV1(old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseNullableObservationV2(oldRaw); err == nil {
		t.Fatal("v2 accepted v1")
	}
	if _, err := CanonicalNullableObservationV2(NullableObservationV2{}); err == nil {
		t.Fatal("zero observation accepted")
	}
	if _, err := CanonicalNullableExplanationV2(NullableExplanationV2{}); err == nil {
		t.Fatal("zero explanation accepted")
	}
}
func TestNullableV2RowsClosedAndOrdered(t *testing.T) {
	_, b, _ := nullableV2Fixture(t)
	cases := map[string]func(*nullableObservationV2){
		"nil rows":      func(v *nullableObservationV2) { v.Graph.Security.DefaultACLRows = nil },
		"nil ACL":       func(v *nullableObservationV2) { v.Graph.Security.DefaultACLRows[0].ACL = nil },
		"private owner": func(v *nullableObservationV2) { v.Graph.Security.DefaultACLRows[0].Owner = "private_role" },
		"private namespace": func(v *nullableObservationV2) {
			s := "private_schema"
			v.Graph.Security.DefaultACLRows[0].Namespace = &s
		},
		"unknown kind":    func(v *nullableObservationV2) { v.Graph.Security.DefaultACLRows[0].ObjectType = "d" },
		"schema scoped":   func(v *nullableObservationV2) { v.Graph.Security.DefaultACLRows[0].ObjectType = "n" },
		"PUBLIC grantor":  func(v *nullableObservationV2) { v.Graph.Security.DefaultACLRows[0].ACL[0].Grantor = "PUBLIC" },
		"unknown grantee": func(v *nullableObservationV2) { v.Graph.Security.DefaultACLRows[0].ACL[0].Grantee = "OTHER" },
		"wrong privilege": func(v *nullableObservationV2) { v.Graph.Security.DefaultACLRows[0].ACL[0].Privilege = "EXECUTE" },
		"duplicate row": func(v *nullableObservationV2) {
			v.Graph.Security.DefaultACLRows[1] = v.Graph.Security.DefaultACLRows[0]
		},
		"row order":   func(v *nullableObservationV2) { r := v.Graph.Security.DefaultACLRows; r[0], r[1] = r[1], r[0] },
		"tuple order": func(v *nullableObservationV2) { r := v.Graph.Security.DefaultACLRows[0].ACL; r[0], r[1] = r[1], r[0] },
		"duplicate tuple option": func(v *nullableObservationV2) {
			r := v.Graph.Security.DefaultACLRows[0].ACL
			r[1] = r[0]
			r[1].Grantable = true
		},
		"snapshot count": func(v *nullableObservationV2) { v.Snapshot.CountOnly["default_acls"]-- },
		"security count": func(v *nullableObservationV2) { v.Graph.Security.DefaultACLCount++ },
		"other acl":      func(v *nullableObservationV2) { v.Graph.Security.OtherACLEntries = 1 },
		"row overflow":   func(v *nullableObservationV2) { v.Graph.Security.DefaultACLRows = make([]nullableDefaultRow, 33) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			v := nullableV2Clone(t, b)
			change(&v.value)
			if _, err := CanonicalNullableObservationV2(v); err == nil {
				t.Fatal("malformed full observation accepted")
			}
		})
	}
	raw, _ := CanonicalNullableObservationV2(b)
	bad := [][]byte{raw[:len(raw)-1], append([]byte(" "), raw...), bytes.Replace(raw, []byte(`"format_version":2`), []byte(`"format_version":2.0`), 1), bytes.Replace(raw, []byte(`"grantable":false`), []byte(`"grantable":null`), 1), bytes.Replace(raw, []byte(`"grantable":false`), []byte(`"grantable":false,"grantable":false`), 1), bytes.Replace(raw, []byte(`"owner":"MIGRATION_OWNER"`), []byte(`"owner":"MIGRATION_OWNER","extra":1`), 1), bytes.Repeat([]byte("x"), nullableObservationLimit+1)}
	for _, r := range bad {
		if _, err := ParseNullableObservationV2(r); err == nil {
			t.Fatal("noncanonical/extra/type/limit accepted")
		}
	}
}
func TestNullableV2UnexpectedCompleteRowsNeverExplain(t *testing.T) {
	op, b, a := nullableV2Fixture(t)
	changes := map[string]func(*nullableObservationV2){
		"option":          func(v *nullableObservationV2) { v.Graph.Security.DefaultACLRows[0].ACL[0].Grantable = true },
		"sequence update": func(v *nullableObservationV2) { v.Graph.Security.DefaultACLRows[0].ACL[0].Privilege = "UPDATE" },
		"PUBLIC":          func(v *nullableObservationV2) { v.Graph.Security.DefaultACLRows[0].ACL[0].Grantee = "PUBLIC" },
		"runtime grantor": func(v *nullableObservationV2) {
			for i := range v.Graph.Security.DefaultACLRows[0].ACL {
				v.Graph.Security.DefaultACLRows[0].ACL[i].Grantor = "RUNTIME"
			}
		},
		"runtime owner": func(v *nullableObservationV2) { v.Graph.Security.DefaultACLRows[0].Owner = "RUNTIME" },
		"function scope": func(v *nullableObservationV2) {
			r := &v.Graph.Security.DefaultACLRows[0]
			r.ObjectType = "f"
			r.ACL = []nullableDefaultGrant{{"MIGRATION_OWNER", "RUNTIME", "EXECUTE", false}}
		},
		"empty stored row": func(v *nullableObservationV2) { v.Graph.Security.DefaultACLRows[0].ACL = []nullableDefaultGrant{} },
		"global":           func(v *nullableObservationV2) { v.Graph.Security.DefaultACLRows[0].Namespace = nil },
		"missing row": func(v *nullableObservationV2) {
			v.Graph.Security.DefaultACLRows = v.Graph.Security.DefaultACLRows[1:]
			v.Graph.Security.DefaultACLCount--
			v.Snapshot.CountOnly["default_acls"]--
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			bb, aa := nullableV2Clone(t, b), nullableV2Clone(t, a)
			change(&bb.value)
			change(&aa.value)
			for _, v := range []*nullableObservationV2{&bb.value, &aa.value} {
				for i := range v.Graph.Security.DefaultACLRows {
					r := v.Graph.Security.DefaultACLRows[i].ACL
					sort.Slice(r, func(i, j int) bool { return nullableDefaultGrantKey(r[i]) < nullableDefaultGrantKey(r[j]) })
				}
				sort.Slice(v.Graph.Security.DefaultACLRows, func(i, j int) bool {
					return nullableDefaultRowKey(v.Graph.Security.DefaultACLRows[i]) < nullableDefaultRowKey(v.Graph.Security.DefaultACLRows[j])
				})
			}
			if _, err := CanonicalNullableObservationV2(bb); err != nil {
				t.Fatal("complete diagnostic rejected", err)
			}
			if _, err := ExplainNullableColumnV2(op, bb, aa); err == nil {
				t.Fatal("equal unexpected defaults qualified")
			}
			if _, err := ExplainNullableColumnV2(op, b, aa); err == nil {
				t.Fatal("changed defaults qualified")
			}
		})
	}
}
func TestNullableV2OtherTransitionAndCaptureGuards(t *testing.T) {
	op, b, a := nullableV2Fixture(t)
	for _, change := range []func(*nullableObservationV2){func(v *nullableObservationV2) { v.Graph.Security.DatabaseACL = strings.Repeat("f", 64) }, func(v *nullableObservationV2) { v.Graph.Security.PublicSchemaACL = strings.Repeat("e", 64) }, func(v *nullableObservationV2) { v.Graph.Slots[len(v.Graph.Slots)-1].NotNull = true }, func(v *nullableObservationV2) { v.Graph.Slots[len(v.Graph.Slots)-1].DefaultPresent = true }} {
		v := nullableV2Clone(t, a)
		change(&v.value)
		if _, err := ExplainNullableColumnV2(op, b, v); err == nil {
			t.Fatal("unexplained delta accepted")
		}
	}
	if _, err := CaptureNullableGraphV2(context.Background(), "invalid-private-dsn", Options{}, NullableColumnOperationV1{}); err == nil || err.Error() != "catalog-snapshot:nullable-operation" {
		t.Fatal("invalid operation reached connection")
	}
	if _, err := CaptureNullableGraphV2(context.Background(), "invalid-private-dsn", Options{IncludeSeeds: true}, op); err == nil || err.Error() != "catalog-snapshot:nullable-capture" {
		t.Fatal("seed option reached connection")
	}
}

// Controlled pgx rows test the physical ACL representation and budgets only;
// they are not positive observations or substitute application qualification.
type nullableV2Rows struct {
	pgx.Rows
	data   [][]any
	at     int
	err    error
	closed bool
}

func (r *nullableV2Rows) Next() bool { r.at++; return r.at <= len(r.data) }
func (r *nullableV2Rows) Scan(dest ...any) error {
	for i, v := range r.data[r.at-1] {
		d := reflect.ValueOf(dest[i]).Elem()
		if v == nil {
			d.Set(reflect.Zero(d.Type()))
		} else {
			d.Set(reflect.ValueOf(v))
		}
	}
	return nil
}
func (r *nullableV2Rows) Err() error { return r.err }
func (r *nullableV2Rows) Close()     { r.closed = true }

type nullableV2Tx struct {
	pgx.Tx
	rows  *nullableV2Rows
	calls int
}

func (t *nullableV2Tx) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	t.calls++
	if sql != nullableDefaultsSQL || len(args) != 0 {
		return nil, errors.New("wrong fixed query")
	}
	return t.rows, nil
}
func nullableV2Ptr[T any](v T) *T { return &v }
func nullableV2Physical() [][]any {
	return [][]any{{int64(100), int64(10), int64(2200), nullableV2Ptr("public"), "S", false, nullableV2Ptr(int64(1)), nullableV2Ptr(int64(1)), nullableV2Ptr(int64(1)), nullableV2Ptr(int64(1)), nullableV2Ptr(int64(1)), false, nullableV2Ptr(int64(10)), nullableV2Ptr(int64(20)), nullableV2Ptr("SELECT"), nullableV2Ptr(false)}}
}
func TestNullableV2PhysicalACLReadCompletenessAndBudget(t *testing.T) {
	cases := map[string]func([][]any) [][]any{
		"duplicate empty row": func(r [][]any) [][]any {
			r[0][6] = nullableV2Ptr(int64(0))
			for _, i := range []int{7, 8, 9, 10, 12, 13, 14, 15} {
				r[0][i] = nil
			}
			r[0][11] = true
			return append(r, append([]any{}, r[0]...))
		},
		"null array":      func(r [][]any) [][]any { r[0][5] = true; return r },
		"null item":       func(r [][]any) [][]any { r[0][11] = true; return r },
		"zero privileges": func(r [][]any) [][]any { r[0][12] = nil; return r },
		"lower bound":     func(r [][]any) [][]any { r[0][8] = nullableV2Ptr(int64(0)); return r },
		"dimension":       func(r [][]any) [][]any { r[0][7] = nullableV2Ptr(int64(2)); return r },
		"missing ordinal": func(r [][]any) [][]any { r[0][10] = nullableV2Ptr(int64(2)); return r },
		"omitted item": func(r [][]any) [][]any {
			r[0][6] = nullableV2Ptr(int64(2))
			r[0][9] = nullableV2Ptr(int64(2))
			return r
		},
		"duplicate tuple": func(r [][]any) [][]any { return append(r, append([]any{}, r[0]...)) },
		"different option duplicate": func(r [][]any) [][]any {
			x := append([]any{}, r[0]...)
			x[15] = nullableV2Ptr(true)
			return append(r, x)
		},
		"duplicate physical pair": func(r [][]any) [][]any {
			r[0][6] = nullableV2Ptr(int64(2))
			r[0][9] = nullableV2Ptr(int64(2))
			x := append([]any{}, r[0]...)
			x[10] = nullableV2Ptr(int64(2))
			x[14] = nullableV2Ptr("USAGE")
			return append(r, x)
		},
		"metadata drift":          func(r [][]any) [][]any { x := append([]any{}, r[0]...); x[1] = int64(20); return append(r, x) },
		"namespace missing":       func(r [][]any) [][]any { r[0][3] = nil; return r },
		"global namespace joined": func(r [][]any) [][]any { r[0][2] = int64(0); return r },
		"unknown owner":           func(r [][]any) [][]any { r[0][1] = int64(30); return r },
		"unknown grantor":         func(r [][]any) [][]any { r[0][12] = nullableV2Ptr(int64(30)); return r },
		"OID overflow":            func(r [][]any) [][]any { r[0][0] = int64(4294967296); return r },
		"item overflow":           func(r [][]any) [][]any { r[0][6] = nullableV2Ptr(int64(257)); return r },
		"row overflow": func(r [][]any) [][]any {
			for i := 1; i < 33; i++ {
				x := append([]any{}, r[0]...)
				x[0] = int64(100 + i)
				r = append(r, x)
			}
			return r
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			tx := &nullableV2Tx{rows: &nullableV2Rows{data: mutate(nullableV2Physical())}}
			if _, _, err := captureNullableDefaults(context.Background(), tx, symbols{"10", "20"}, 1<<20); err == nil {
				t.Fatal("incomplete physical representation accepted")
			}
			if !tx.rows.closed || tx.calls != 1 {
				t.Fatal("query ownership changed")
			}
		})
	}
	for _, n := range []int{0, 130} {
		tx := &nullableV2Tx{rows: &nullableV2Rows{data: nullableV2Physical()}}
		if _, _, err := captureNullableDefaults(context.Background(), tx, symbols{"10", "20"}, n); err == nil {
			t.Fatal("budget reset")
		}
	}
	tx := &nullableV2Tx{rows: &nullableV2Rows{data: nullableV2Physical(), err: errors.New("private transport text")}}
	if _, _, err := captureNullableDefaults(context.Background(), tx, symbols{"10", "20"}, 1<<20); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("partial/error stream accepted or exposed")
	}
}
func TestNullableV2PhysicalSymbolicOrderAndEmptyRows(t *testing.T) {
	read := func(data [][]any, roles symbols) []nullableDefaultRow {
		tx := &nullableV2Tx{rows: &nullableV2Rows{data: data}}
		r, remaining, err := captureNullableDefaults(context.Background(), tx, roles, 1<<20)
		if err != nil || remaining >= 1<<20 {
			t.Fatal("physical read failed", err)
		}
		return r
	}
	a := read(nullableV2Physical(), symbols{"10", "20"})
	changed := nullableV2Physical()
	changed[0][0] = int64(800)
	changed[0][1] = int64(40)
	changed[0][12] = nullableV2Ptr(int64(40))
	changed[0][13] = nullableV2Ptr(int64(50))
	b := read(changed, symbols{"40", "50"})
	if !reflect.DeepEqual(a, b) {
		t.Fatal("OID leaked into symbolic rows")
	}
	empty := nullableV2Physical()
	empty[0][6] = nullableV2Ptr(int64(0))
	for _, i := range []int{7, 8, 9, 10, 12, 13, 14, 15} {
		empty[0][i] = nil
	}
	empty[0][11] = true
	r := read(empty, symbols{"10", "20"})
	if len(r) != 1 || r[0].ACL == nil || len(r[0].ACL) != 0 {
		t.Fatal("empty stored row lost")
	}
}

func nullableV2AST(n ast.Node) string {
	var b bytes.Buffer
	_ = printer.Fprint(&b, token.NewFileSet(), n)
	return b.String()
}
func nullableV2Statements(raw string) []ast.Stmt {
	f, e := parser.ParseFile(token.NewFileSet(), "expected.go", "package p\nfunc expected(){"+raw+"}", 0)
	if e != nil {
		panic(e)
	}
	return f.Decls[0].(*ast.FuncDecl).Body.List
}

// These tests bind the reviewed direct SQL writers, not arbitrary dynamic SQL.
// Immutable source review and the complete actual catalog observation remain required.
type nullableV2SourceSites struct {
	function             *ast.FuncDecl
	transaction          *ast.FuncLit
	transactionStatement *ast.IfStmt
	roles                map[string]*ast.AssignStmt
	statements           *ast.AssignStmt
	grants               []ast.Stmt
	writers              map[ast.Expr]bool
}

func nullableV2TemplateStatements() []ast.Stmt {
	templates := []string{
		`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public REVOKE ALL PRIVILEGES ON TABLES FROM %s`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public REVOKE ALL PRIVILEGES ON TABLES FROM PUBLIC`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO %s`,
	}
	var result []ast.Stmt
	for i, v := range templates {
		args := "migrator,runtime"
		if i == 1 {
			args = "migrator"
		}
		result = append(result, nullableV2Statements(`if err := tx.Exec(fmt.Sprintf("` + v + `",` + args + `)).Error; err != nil {return err}`)[0])
	}
	return result
}

func nullableV2Sprintf(statement ast.Stmt) *ast.CallExpr {
	check := statement.(*ast.IfStmt)
	errorField := check.Init.(*ast.AssignStmt).Rhs[0].(*ast.SelectorExpr)
	return errorField.X.(*ast.CallExpr).Args[0].(*ast.CallExpr)
}

func nullableV2SourceSitesFor(f *ast.File, preparation bool) *nullableV2SourceSites {
	fmtOK, sqlOK := false, false
	for _, i := range f.Imports {
		if i.Path.Value == `"fmt"` && (i.Name == nil || i.Name.Name == "fmt") {
			fmtOK = true
		}
		if i.Path.Value == `"database/sql"` && (i.Name == nil || i.Name.Name == "sql") {
			sqlOK = true
		}
	}
	if !fmtOK || (!preparation && !sqlOK) {
		return nil
	}
	name := "ApplyTenantRLS"
	if preparation {
		name = "revokeRuntimeDefaultTablePrivilegesBeforeAdditivePreparation"
	}
	sites := &nullableV2SourceSites{roles: map[string]*ast.AssignStmt{}, writers: map[ast.Expr]bool{}}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == name {
			if sites.function != nil {
				return nil
			}
			sites.function = fn
		}
	}
	if sites.function == nil || sites.function.Body == nil || sites.function.Recv != nil || nullableV2AST(sites.function.Type) != "func(db *gorm.DB, runtimeRole string) error" {
		return nil
	}
	for _, s := range sites.function.Body.List {
		for role, source := range map[string]string{"migrator": "migrationRole", "runtime": "runtimeRole"} {
			want := nullableV2AST(nullableV2Statements(role + " := quoteIdentifier(" + source + ")")[0])
			if nullableV2AST(s) == want {
				if sites.roles[role] != nil {
					return nil
				}
				sites.roles[role] = s.(*ast.AssignStmt)
			}
		}
		check, ok := s.(*ast.IfStmt)
		if !ok {
			continue
		}
		assign, ok := check.Init.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 || len(assign.Lhs) != 1 || assign.Tok != token.DEFINE || nullableV2AST(assign.Lhs[0]) != "err" {
			continue
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok || nullableV2AST(call.Fun) != "db.Transaction" {
			continue
		}
		// The application transaction explicitly binds READ COMMITTED; the
		// preparation transaction uses the existing one-argument API.
		if preparation {
			if len(call.Args) != 1 {
				return nil
			}
		} else if len(call.Args) != 2 || nullableV2AST(call.Args[1]) != "&sql.TxOptions{Isolation: sql.LevelReadCommitted}" {
			return nil
		}
		lit, ok := call.Args[0].(*ast.FuncLit)
		if !ok || nullableV2AST(lit.Type) != "func(tx *gorm.DB) error" || nullableV2AST(check.Cond) != "err != nil" || check.Else != nil {
			return nil
		}
		if sites.transaction != nil {
			return nil
		}
		sites.transaction, sites.transactionStatement = lit, check
	}
	if sites.transaction == nil || len(sites.roles) != 2 {
		return nil
	}
	for _, initializer := range sites.roles {
		if initializer.End() >= sites.transactionStatement.Pos() {
			return nil
		}
	}
	for _, s := range sites.function.Body.List {
		if s == sites.transactionStatement {
			break
		}
		switch s.(type) {
		case *ast.ReturnStmt, *ast.BranchStmt:
			return nil
		}
	}
	expected := nullableV2TemplateStatements()
	if preparation {
		first, second := nullableV2Sprintf(expected[0]), nullableV2Sprintf(expected[1])
		want := nullableV2AST(nullableV2Statements("statements := []string{" + nullableV2AST(first) + "," + nullableV2AST(second) + "}")[0])
		for _, s := range sites.function.Body.List {
			if nullableV2AST(s) == want {
				if sites.statements != nil {
					return nil
				}
				sites.statements = s.(*ast.AssignStmt)
			}
		}
		if sites.statements == nil || sites.statements.End() >= sites.transactionStatement.Pos() {
			return nil
		}
		for _, initializer := range sites.roles {
			if initializer.End() >= sites.statements.Pos() {
				return nil
			}
		}
		body := nullableV2Statements(`for _, statement := range statements {if err := tx.Exec(statement).Error; err != nil {return fmt.Errorf("revoke additive-preparation default table privileges: %w",err)}};return verifyRuntimeDefaultTablePrivilegesRevoked(tx,migrationRoleOID,runtimeRoleOID)`)
		if len(sites.transaction.Body.List) != len(body) {
			return nil
		}
		for i, s := range body {
			if nullableV2AST(s) != nullableV2AST(sites.transaction.Body.List[i]) {
				return nil
			}
		}
		for _, e := range sites.statements.Rhs[0].(*ast.CompositeLit).Elts {
			sites.writers[e.(*ast.CallExpr).Args[0]] = true
		}
	} else {
		for i := 0; i+len(expected) <= len(sites.transaction.Body.List); i++ {
			match := true
			for j, s := range expected {
				if nullableV2AST(s) != nullableV2AST(sites.transaction.Body.List[i+j]) {
					match = false
				}
			}
			if match {
				if sites.grants != nil {
					return nil
				}
				sites.grants = sites.transaction.Body.List[i : i+len(expected)]
			}
		}
		if sites.grants == nil {
			return nil
		}
		for _, s := range sites.grants {
			sites.writers[nullableV2Sprintf(s).Args[0]] = true
		}
		// An unconditional exit before the direct writer sequence is not execution.
		for _, s := range sites.transaction.Body.List {
			if s == sites.grants[0] {
				break
			}
			switch s.(type) {
			case *ast.ReturnStmt, *ast.BranchStmt:
				return nil
			}
		}
	}
	if !nullableV2SourceBindingsStable(sites, preparation) {
		return nil
	}
	return sites
}

func nullableV2SourceBindingsStable(sites *nullableV2SourceSites, preparation bool) bool {
	// Exact initializer/parameter nodes are the only permitted bindings. This
	// intentionally rejects even nested rebindings instead of guessing reachability.
	allowed := map[ast.Node]bool{sites.roles["migrator"]: true, sites.roles["runtime"]: true, sites.transaction.Type.Params.List[0]: true}
	if preparation {
		allowed[sites.statements] = true
	}
	start := sites.roles["migrator"].Pos()
	if sites.roles["runtime"].Pos() < start {
		start = sites.roles["runtime"].Pos()
	}
	reserved := func(name string, pos token.Pos) bool {
		switch name {
		case "runtime", "migrator", "tx", "fmt", "sql", "quoteIdentifier":
			return true
		case "statements":
			return preparation
		case "runtimeRole", "migrationRole", "db":
			return pos >= start
		}
		return false
	}
	touches := func(n ast.Node) bool {
		found := false
		ast.Inspect(n, func(v ast.Node) bool {
			if id, ok := v.(*ast.Ident); ok && reserved(id.Name, id.Pos()) {
				found = true
			}
			return true
		})
		return found
	}
	valid := true
	ast.Inspect(sites.function.Body, func(n ast.Node) bool {
		if !valid || n == nil {
			return false
		}
		if !allowed[n] {
			switch q := n.(type) {
			case *ast.AssignStmt:
				for _, left := range q.Lhs {
					if touches(left) {
						valid = false
					}
				}
			case *ast.ValueSpec:
				for _, id := range q.Names {
					if reserved(id.Name, id.Pos()) {
						valid = false
					}
				}
			case *ast.TypeSpec:
				if reserved(q.Name.Name, q.Name.Pos()) {
					valid = false
				}
			case *ast.Field:
				for _, id := range q.Names {
					if reserved(id.Name, id.Pos()) {
						valid = false
					}
				}
			case *ast.RangeStmt:
				if touches(q.Key) || touches(q.Value) {
					valid = false
				}
			case *ast.IncDecStmt:
				if touches(q.X) {
					valid = false
				}
			case *ast.UnaryExpr:
				// Taking the address of a new options literal does not expose a
				// reserved local. Nested &tx or &runtime expressions are visited.
				_, newValue := q.X.(*ast.CompositeLit)
				if q.Op == token.AND && !newValue && touches(q.X) {
					valid = false
				}
			}
		}
		if preparation {
			// Do not permit a slice alias, escaped slice, append, or hidden mutation.
			if id, ok := n.(*ast.Ident); ok && id.Name == "statements" {
				declaration := sites.statements.Lhs[0]
				rangeSource := sites.transaction.Body.List[0].(*ast.RangeStmt).X
				if id != declaration && id != rangeSource {
					valid = false
				}
			}
		}
		return valid
	})
	return valid
}

func nullableV2ConstantString(e ast.Expr, depth int) (string, bool) {
	if depth > 64 {
		return "", false
	}
	switch q := e.(type) {
	case *ast.BasicLit:
		if q.Kind == token.STRING {
			value, err := strconv.Unquote(q.Value)
			return value, err == nil
		}
	case *ast.ParenExpr:
		return nullableV2ConstantString(q.X, depth+1)
	case *ast.BinaryExpr:
		if q.Op == token.ADD {
			left, lok := nullableV2ConstantString(q.X, depth+1)
			right, rok := nullableV2ConstantString(q.Y, depth+1)
			if lok && rok && len(left)+len(right) <= 1<<20 {
				return left + right, true
			}
		}
	}
	return "", false
}

func nullableV2SourceInventory(sources map[string][]byte) bool {
	// Bounds are for this static test inventory, not runtime source admission.
	if len(sources) > 2048 {
		return false
	}
	known := map[string]bool{"internal/database/tenant.go": false, "internal/database/postgres.go": true}
	seenFiles, totalBytes, totalWriters := 0, 0, 0
	for path, raw := range sources {
		if !(strings.HasPrefix(path, "internal/") || strings.HasPrefix(path, "cmd/") || strings.HasPrefix(path, "pkg/")) || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return false
		}
		totalBytes += len(raw)
		if len(raw) > 8<<20 || totalBytes > 64<<20 {
			return false
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, raw, 0)
		if err != nil {
			return false
		}
		writers := map[ast.Expr]bool{}
		if preparation, ok := known[path]; ok {
			sites := nullableV2SourceSitesFor(f, preparation)
			if sites == nil {
				return false
			}
			writers = sites.writers
			seenFiles++
		}
		valid := true
		ast.Inspect(f, func(n ast.Node) bool {
			if !valid || n == nil {
				return false
			}
			if e, ok := n.(ast.Expr); ok {
				if value, constant := nullableV2ConstantString(e, 0); constant {
					if strings.Contains(strings.ToUpper(strings.Join(strings.Fields(value), " ")), "ALTER DEFAULT PRIVILEGES") {
						if !writers[e] {
							valid = false
							return false
						}
						delete(writers, e)
						totalWriters++
					}
					return false // one maximal decoded expression, never double-count fragments
				}
			}
			return true
		})
		if !valid || len(writers) != 0 {
			return false
		}
	}
	return seenFiles == 2 && totalWriters == 6
}

func nullableV2ProductionSources(t *testing.T) map[string][]byte {
	t.Helper()
	sources := map[string][]byte{}
	root := filepath.Join("..", "..")
	total := int64(0)
	for _, directory := range []string{"internal", "cmd", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return errors.New("source inventory symlink")
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			info, e := d.Info()
			if e != nil {
				return e
			}
			total += info.Size()
			if !info.Mode().IsRegular() || info.Size() > 8<<20 || total > 64<<20 || len(sources) >= 2048 {
				return errors.New("source inventory bounds")
			}
			raw, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			rel, e := filepath.Rel(root, path)
			if e != nil {
				return e
			}
			sources[filepath.ToSlash(rel)] = raw
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return sources
}

func TestNullableV2SourceDefaultACLContract(t *testing.T) {
	rows, err := nullableExpectedDefaultRows()
	if err != nil || len(rows) != 2 {
		t.Fatal("constant profile/hash drift")
	}
	if !nullableV2SourceInventory(nullableV2ProductionSources(t)) {
		t.Fatal("executed source default-privilege inventory changed")
	}
}

func TestNullableV2SourceDefaultACLContractMutants(t *testing.T) {
	sources := nullableV2ProductionSources(t)
	minimal := map[string][]byte{"internal/database/tenant.go": sources["internal/database/tenant.go"], "internal/database/postgres.go": sources["internal/database/postgres.go"]}
	if !nullableV2SourceInventory(minimal) {
		t.Fatal("source fixture is not the actual reviewed pair")
	}
	mutate := func(t *testing.T, preparation bool, change func(*ast.File, *nullableV2SourceSites)) []byte {
		t.Helper()
		path := "internal/database/tenant.go"
		if preparation {
			path = "internal/database/postgres.go"
		}
		f, e := parser.ParseFile(token.NewFileSet(), path, minimal[path], 0)
		if e != nil {
			t.Fatal(e)
		}
		sites := nullableV2SourceSitesFor(f, preparation)
		if sites == nil {
			t.Fatal("original execution bindings missing")
		}
		change(f, sites)
		return []byte(nullableV2AST(f))
	}
	beforeTransaction := func(s *nullableV2SourceSites, raw string) {
		for i, statement := range s.function.Body.List {
			if statement == s.transactionStatement {
				prefix := append([]ast.Stmt{}, s.function.Body.List[:i]...)
				prefix = append(prefix, nullableV2Statements(raw)...)
				s.function.Body.List = append(prefix, s.function.Body.List[i:]...)
				return
			}
		}
		panic("transaction missing")
	}
	beforeGrant := func(s *nullableV2SourceSites, raw string) {
		s.transaction.Body.List = append(nullableV2Statements(raw), s.transaction.Body.List...)
	}
	cases := []struct {
		name        string
		preparation bool
		change      func(*ast.File, *nullableV2SourceSites)
	}{
		{"dead-original-changed-grant", false, func(f *ast.File, s *nullableV2SourceSites) {
			call := nullableV2Sprintf(s.grants[2])
			old := call.Args[0].(*ast.BasicLit).Value
			call.Args[0] = &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote("ALTER DEFAULT PRIVILEGES FOR ROLE %s IN SCHEMA public GRANT ALL ON TABLES TO %s")}
			f.Decls = append(f.Decls, &ast.GenDecl{Tok: token.VAR, Specs: []ast.Spec{&ast.ValueSpec{Names: []*ast.Ident{ast.NewIdent("unusedOriginal")}, Values: []ast.Expr{&ast.BasicLit{Kind: token.STRING, Value: old}}}}})
		}},
		{"wrong-role-args", false, func(_ *ast.File, s *nullableV2SourceSites) {
			nullableV2Sprintf(s.grants[2]).Args[1] = ast.NewIdent("runtime")
		}},
		{"role-reassignment", false, func(_ *ast.File, s *nullableV2SourceSites) { beforeTransaction(s, `runtime = "PUBLIC"`) }},
		{"migrator-reassignment", false, func(_ *ast.File, s *nullableV2SourceSites) { beforeTransaction(s, `migrator = "PUBLIC"`) }},
		{"role-shadow", false, func(_ *ast.File, s *nullableV2SourceSites) { beforeGrant(s, `runtime := "PUBLIC"; _ = runtime`) }},
		{"role-address-escape", false, func(_ *ast.File, s *nullableV2SourceSites) { beforeTransaction(s, `mutate(&migrator)`) }},
		{"executor-reassignment", false, func(_ *ast.File, s *nullableV2SourceSites) { beforeGrant(s, `tx = db`) }},
		{"executor-shadow", false, func(_ *ast.File, s *nullableV2SourceSites) { beforeGrant(s, `{tx := db; _ = tx}`) }},
		{"executor-address-escape", false, func(_ *ast.File, s *nullableV2SourceSites) { beforeGrant(s, `mutate(&tx)`) }},
		{"fmt-shadow", false, func(_ *ast.File, s *nullableV2SourceSites) { beforeGrant(s, `fmt := fakeFormatter; _ = fmt`) }},
		{"sql-shadow", false, func(_ *ast.File, s *nullableV2SourceSites) { beforeTransaction(s, `sql := fakeSQL; _ = sql`) }},
		{"transaction-option-removed", false, func(_ *ast.File, s *nullableV2SourceSites) {
			call := s.transactionStatement.Init.(*ast.AssignStmt).Rhs[0].(*ast.CallExpr)
			call.Args = call.Args[:1]
		}},
		{"transaction-option-changed", false, func(_ *ast.File, s *nullableV2SourceSites) {
			call := s.transactionStatement.Init.(*ast.AssignStmt).Rhs[0].(*ast.CallExpr)
			call.Args[1] = ast.NewIdent("unreviewedOptions")
		}},
		{"wrong-executor", false, func(_ *ast.File, s *nullableV2SourceSites) {
			q := s.grants[0].(*ast.IfStmt).Init.(*ast.AssignStmt).Rhs[0].(*ast.SelectorExpr)
			q.X.(*ast.CallExpr).Fun.(*ast.SelectorExpr).X = ast.NewIdent("db")
		}},
		{"early-return", false, func(_ *ast.File, s *nullableV2SourceSites) { beforeGrant(s, `return nil`) }},
		{"reorder", false, func(_ *ast.File, s *nullableV2SourceSites) { s.grants[0], s.grants[1] = s.grants[1], s.grants[0] }},
		{"duplicate", false, func(_ *ast.File, s *nullableV2SourceSites) {
			s.transaction.Body.List = append(s.transaction.Body.List, s.grants[0])
		}},
		{"remove", false, func(_ *ast.File, s *nullableV2SourceSites) {
			for i, q := range s.transaction.Body.List {
				if q == s.grants[0] {
					s.transaction.Body.List = append(s.transaction.Body.List[:i], s.transaction.Body.List[i+1:]...)
					break
				}
			}
		}},
		{"prep-role-reassignment", true, func(_ *ast.File, s *nullableV2SourceSites) { beforeTransaction(s, `runtime = "PUBLIC"`) }},
		{"prep-slice-reassignment", true, func(_ *ast.File, s *nullableV2SourceSites) { beforeTransaction(s, `statements = nil`) }},
		{"prep-slice-item-write", true, func(_ *ast.File, s *nullableV2SourceSites) { beforeTransaction(s, `statements[0] = "SELECT 1"`) }},
		{"prep-slice-alias", true, func(_ *ast.File, s *nullableV2SourceSites) {
			beforeTransaction(s, `alias := statements; mutate(alias)`)
		}},
		{"prep-slice-shadow", true, func(_ *ast.File, s *nullableV2SourceSites) {
			beforeGrant(s, `statements := []string{}; _ = statements`)
		}},
		{"prep-executor-reassignment", true, func(_ *ast.File, s *nullableV2SourceSites) { beforeGrant(s, `tx = db`) }},
		{"prep-wrong-range", true, func(_ *ast.File, s *nullableV2SourceSites) {
			s.transaction.Body.List[0].(*ast.RangeStmt).X = ast.NewIdent("unrelated")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := "internal/database/tenant.go"
			if tc.preparation {
				path = "internal/database/postgres.go"
			}
			changed := map[string][]byte{}
			for k, v := range minimal {
				changed[k] = v
			}
			changed[path] = mutate(t, tc.preparation, tc.change)
			if nullableV2SourceInventory(changed) {
				t.Fatal("changed execution/source authority accepted")
			}
		})
	}
	for _, root := range []string{"internal", "cmd", "pkg"} {
		t.Run("additional-writer-"+root, func(t *testing.T) {
			changed := map[string][]byte{}
			for k, v := range minimal {
				changed[k] = v
			}
			// Decoding handles escaped spaces, mixed case, and split constant strings.
			changed[root+"/unreviewed.go"] = []byte("package p\nvar extra = (\"aLtEr\\x20\" + \"default\\t\" + `privileges FOR ROLE x GRANT ALL ON TABLES TO PUBLIC`)")
			if nullableV2SourceInventory(changed) {
				t.Fatal("extra decoded writer accepted")
			}
		})
	}
	t.Run("harmless-format-and-comment", func(t *testing.T) {
		changed := map[string][]byte{}
		for path, raw := range minimal {
			changed[path] = append([]byte("// ALTER DEFAULT PRIVILEGES in a comment is not an executed writer.\n\n"), raw...)
		}
		if !nullableV2SourceInventory(changed) {
			t.Fatal("comments/formatting changed execution contract")
		}
		for path, raw := range changed {
			f, e := parser.ParseFile(token.NewFileSet(), path, raw, 0)
			if e != nil {
				t.Fatal(e)
			}
			changed[path] = []byte(nullableV2AST(f))
		}
		if !nullableV2SourceInventory(changed) {
			t.Fatal("AST-equivalent formatting changed contract")
		}
	})
}
