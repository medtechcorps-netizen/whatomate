package registry

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/shridarpatil/whatomate/internal/models"
)

// literalReader evaluates only the committed literal shapes needed by seed
// definitions. It executes no seed function, reflection-based application
// callback, SQL, or arbitrary Go expression. A changed unsupported form fails.
type literalReader struct {
	constants map[string]ast.Expr
	structs   map[string][]string
}

func newLiteralReader(files ...*ast.File) literalReader {
	r := literalReader{map[string]ast.Expr{}, map[string][]string{}}
	for _, f := range files {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, s := range g.Specs {
				switch v := s.(type) {
				case *ast.ValueSpec:
					if g.Tok != token.CONST || len(v.Names) != len(v.Values) {
						continue
					}
					for i, n := range v.Names {
						r.constants[n.Name] = v.Values[i]
					}
				case *ast.TypeSpec:
					if st, ok := v.Type.(*ast.StructType); ok {
						fields := []string{}
						for _, field := range st.Fields.List {
							for _, n := range field.Names {
								fields = append(fields, n.Name)
							}
						}
						r.structs[v.Name.Name] = fields
					}
				}
			}
		}
	}
	return r
}
func (r literalReader) value(expr ast.Expr, env map[string]any) (any, error) {
	return r.eval(expr, env, 0)
}
func (r literalReader) eval(expr ast.Expr, env map[string]any, depth int) (any, error) {
	if depth > 64 {
		return nil, fmt.Errorf("seed literal nesting exceeds bound")
	}
	next := func(e ast.Expr) (any, error) { return r.eval(e, env, depth+1) }
	switch x := expr.(type) {
	case *ast.BasicLit:
		switch x.Kind {
		case token.STRING:
			return strconv.Unquote(x.Value)
		case token.INT:
			return strconv.ParseInt(x.Value, 0, 64)
		}
	case *ast.Ident:
		if v, ok := env[x.Name]; ok {
			return v, nil
		}
		switch x.Name {
		case "nil":
			return nil, nil
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		if e, ok := r.constants[x.Name]; ok {
			return next(e)
		}
	case *ast.SelectorExpr:
		v, err := next(x.X)
		if err != nil {
			return nil, err
		}
		if m, ok := v.(map[string]any); ok {
			v, ok := m[x.Sel.Name]
			if ok {
				return v, nil
			}
		}
	case *ast.BinaryExpr:
		left, err := next(x.X)
		if err != nil {
			return nil, err
		}
		right, err := next(x.Y)
		if err != nil {
			return nil, err
		}
		if x.Op == token.EQL {
			return reflect.DeepEqual(left, right), nil
		}
		if x.Op == token.ADD {
			a, ok := left.(string)
			b, bok := right.(string)
			if ok && bok {
				return a + b, nil
			}
		}
	case *ast.CompositeLit:
		var fields []string
		switch typ := x.Type.(type) {
		case *ast.Ident:
			fields = r.structs[typ.Name]
		case *ast.StructType:
			for _, f := range typ.Fields.List {
				for _, n := range f.Names {
					fields = append(fields, n.Name)
				}
			}
		}
		if len(fields) > 0 {
			result := map[string]any{}
			for i, e := range x.Elts {
				name := ""
				value := e
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					id, ok := kv.Key.(*ast.Ident)
					if !ok {
						return nil, fmt.Errorf("non-field struct key")
					}
					name = id.Name
					value = kv.Value
				} else {
					if i >= len(fields) {
						return nil, fmt.Errorf("too many struct fields")
					}
					name = fields[i]
				}
				known := false
				for _, field := range fields {
					known = known || field == name
				}
				if !known {
					return nil, fmt.Errorf("unknown struct field")
				}
				if _, exists := result[name]; exists {
					return nil, fmt.Errorf("duplicate field")
				}
				v, err := next(value)
				if err != nil {
					return nil, err
				}
				result[name] = v
			}
			if len(result) != len(fields) {
				return nil, fmt.Errorf("seed struct omitted fields")
			}
			return result, nil
		}
		isMap := false
		switch typ := x.Type.(type) {
		case *ast.MapType:
			isMap = true
		case *ast.SelectorExpr:
			isMap = formatted(typ) == "models.JSONB"
		}
		if isMap {
			result := map[string]any{}
			for _, e := range x.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					return nil, fmt.Errorf("unkeyed map")
				}
				key, err := next(kv.Key)
				if err != nil {
					return nil, err
				}
				name, ok := key.(string)
				if !ok {
					return nil, fmt.Errorf("non-string map key")
				}
				if _, exists := result[name]; exists {
					return nil, fmt.Errorf("duplicate map key")
				}
				v, err := next(kv.Value)
				if err != nil {
					return nil, err
				}
				result[name] = v
			}
			return result, nil
		}
		if x.Type == nil && len(x.Elts) == 0 {
			return map[string]any{}, nil
		}
		if _, ok := x.Type.(*ast.ArrayType); ok {
			result := []any{}
			for _, e := range x.Elts {
				if inner, ok := e.(*ast.CompositeLit); ok && inner.Type == nil {
					copy := *inner
					copy.Type = x.Type.(*ast.ArrayType).Elt
					e = &copy
				}
				v, err := next(e)
				if err != nil {
					return nil, err
				}
				result = append(result, v)
			}
			return result, nil
		}
	}
	return nil, fmt.Errorf("unsupported seed literal %T: %s", expr, formatted(expr))
}

func mustLiteral(t *testing.T, r literalReader, expr ast.Expr, env map[string]any) any {
	t.Helper()
	v, err := r.value(expr, env)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func localValue(t *testing.T, fn *ast.FuncDecl, name string) ast.Expr {
	t.Helper()
	var result ast.Expr
	count := 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		a, ok := n.(*ast.AssignStmt)
		if !ok || a.Tok != token.DEFINE {
			return true
		}
		for i, l := range a.Lhs {
			if id, ok := l.(*ast.Ident); ok && id.Name == name {
				if len(a.Lhs) != len(a.Rhs) {
					t.Fatal("unsupported local binding")
				}
				result = a.Rhs[i]
				count++
			}
		}
		return true
	})
	if count != 1 {
		t.Fatalf("local %s binding count %d", name, count)
	}
	return result
}

func widgetSeeds(t *testing.T, f *ast.File, r literalReader) []map[string]any {
	t.Helper()
	fn := sourceFunction(t, f, "SeedDefaultWidgetsForOrg")
	rows, ok := mustLiteral(t, r, localValue(t, fn, "defaultWidgetsData"), nil).([]any)
	if !ok {
		t.Fatal("widget rows shape")
	}
	var loop *ast.RangeStmt
	for _, s := range fn.Body.List {
		if v, ok := s.(*ast.RangeStmt); ok {
			if loop != nil {
				t.Fatal("multiple widget ranges")
			}
			loop = v
		}
	}
	if loop == nil || formatted(loop.X) != "defaultWidgetsData" || formatted(loop.Value) != "wd" || len(loop.Body.List) != 4 {
		t.Fatal("widget constructor flow changed")
	}
	if formatted(loop.Body.List[0]) != "displayType := wd.DisplayType" || formatted(loop.Body.List[1]) != "if displayType == \"\" {\n\tdisplayType = \"number\"\n}" {
		t.Fatal("widget display-type normalization changed; extend the source evaluator explicitly")
	}
	constructor, ok := localValue(t, fn, "widget").(*ast.CompositeLit)
	if !ok || formatted(constructor.Type) != "models.Widget" {
		t.Fatal("widget constructor shape")
	}
	result := []map[string]any{}
	for _, row := range rows {
		wd, ok := row.(map[string]any)
		if !ok {
			t.Fatal("widget row type")
		}
		display, ok := wd["DisplayType"].(string)
		if !ok {
			t.Fatal("widget display type")
		}
		if display == "" {
			display = "number"
		}
		env := map[string]any{"wd": wd, "displayType": display}
		out := map[string]any{}
		identities := map[string]string{"BaseModel": "models.BaseModel{ID: uuid.New()}", "OrganizationID": "orgID", "UserID": "&userID"}
		seen := map[string]bool{}
		for _, e := range constructor.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				t.Fatal("unkeyed widget constructor")
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				t.Fatal("widget constructor key")
			}
			if seen[key.Name] {
				t.Fatal("duplicate widget field")
			}
			seen[key.Name] = true
			if expected, identity := identities[key.Name]; identity {
				if formatted(kv.Value) != expected {
					t.Fatal("widget identity expression changed")
				}
				continue
			}
			out[key.Name] = mustLiteral(t, r, kv.Value, env)
		}
		for key := range identities {
			if !seen[key] {
				t.Fatal("widget identity binding omitted")
			}
		}
		result = append(result, out)
	}
	return result
}

type permissionSeed struct {
	Resource    string `json:"resource"`
	Action      string `json:"action"`
	Description string `json:"description"`
}
type seedsManifest struct {
	Version                 int                 `json:"version"`
	Permissions             []permissionSeed    `json:"permissions"`
	SystemRolePermissions   map[string][]string `json:"system_role_permissions"`
	SystemRoleDefinitions   any                 `json:"system_role_definitions"`
	CatalogConstants        map[string]any      `json:"catalog_constants"`
	ProductCatalog          any                 `json:"product_catalog"`
	WidgetDefaults          []map[string]any    `json:"widget_defaults"`
	WidgetRuntimeIdentities map[string]string   `json:"widget_runtime_identities"`
	ManagerPolicy           map[string]any      `json:"manager_policy"`
}

func generatedSeeds(t *testing.T) seedsManifest {
	t.Helper()
	root := registryRoot(t)
	postgres := parseSource(t, root, "internal/database/postgres.go")
	product := parseSource(t, root, "internal/database/product_catalog.go")
	manager := parseSource(t, root, "internal/database/manager_settings_policy.go")
	r := newLiteralReader(postgres, product, manager)
	// An added model field must be reviewed here rather than silently omitted
	// from the generated permission definitions. BaseModel remains excluded
	// only while every returned identity/timestamp field is the zero value.
	permissionType := reflect.TypeFor[models.Permission]()
	permissionFields := []string{}
	for i := 0; i < permissionType.NumField(); i++ {
		permissionFields = append(permissionFields, permissionType.Field(i).Name)
	}
	if !reflect.DeepEqual(permissionFields, []string{"BaseModel", "Resource", "Action", "Description"}) {
		t.Fatal("permission model fields changed; review seed serialization")
	}
	permissions := []permissionSeed{}
	keys := map[string]bool{}
	for _, p := range models.DefaultPermissions() {
		if !reflect.DeepEqual(p.BaseModel, models.BaseModel{}) {
			t.Fatal("permission seeds unexpectedly contain row identities")
		}
		key := p.Resource + ":" + p.Action
		if keys[key] {
			t.Fatal("duplicate permission key")
		}
		keys[key] = true
		permissions = append(permissions, permissionSeed{p.Resource, p.Action, p.Description})
	}
	roles := models.SystemRolePermissions()
	for role, list := range roles {
		seen := map[string]bool{}
		for _, key := range list {
			if !keys[key] || seen[key] {
				t.Fatalf("invalid permission for role %s", role)
			}
			seen[key] = true
		}
	}
	constants := map[string]any{}
	for _, d := range product.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, s := range g.Specs {
			v, ok := s.(*ast.ValueSpec)
			if !ok || len(v.Names) != len(v.Values) {
				t.Fatal("unsupported catalog constant group")
			}
			for i, n := range v.Names {
				constants[n.Name] = mustLiteral(t, r, v.Values[i], nil)
			}
		}
	}
	constants["PlatformResellerSlug"] = mustLiteral(t, r, sourceValue(t, postgres, "PlatformResellerSlug"), nil)
	policy := map[string]any{"version": mustLiteral(t, r, sourceValue(t, manager, "ManagerSettingsPolicyVersion"), nil), "version_key": mustLiteral(t, r, sourceValue(t, manager, "ManagerSettingsPolicyVersionKey"), nil), "retired_permissions": mustLiteral(t, r, sourceValue(t, manager, "retiredManagerSettingsPermissions"), nil)}
	return seedsManifest{0, permissions, roles, mustLiteral(t, r, localValue(t, sourceFunction(t, postgres, "seedSystemRolesForOrg"), "systemRoles"), nil), constants, mustLiteral(t, r, sourceValue(t, product, "reReplyProductCatalog"), nil), widgetSeeds(t, postgres, r), map[string]string{"BaseModel.ID": "new UUID at execution; excluded from static seeds", "OrganizationID": "organization argument", "UserID": "user argument pointer"}, policy}
}
func TestSeedsManifestMatchesDefinitions(t *testing.T) {
	compareOrUpdate(t, "../golden/seeds.json", "REREPLY_UPDATE_SEEDS", generatedSeeds(t))
}

func TestSeedLiteralReaderFailsClosed(t *testing.T) {
	r := literalReader{map[string]ast.Expr{}, map[string][]string{}}
	for _, input := range []string{"loadFromDatabase()", "time.Now()", "[]int{1: 3}", "map[string]any{\"key\": 1, \"key\": 2}", "unrecognizedIdentifier"} {
		e, err := parseSeedExpr(input)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.value(e, nil); err == nil {
			t.Fatalf("unsupported seed form accepted: %s", input)
		}
	}
}

func TestSeedManifestIncludesSourceDefinitionChanges(t *testing.T) {
	// Pure local AST variants prove nested package data are not dropped.
	r := literalReader{map[string]ast.Expr{"version": &ast.BasicLit{Kind: token.INT, Value: "1"}}, map[string][]string{}}
	first, _ := parseSeedExpr(`map[string]any{"policy": version, "flags": map[string]bool{"enabled": false}}`)
	second, _ := parseSeedExpr(`map[string]any{"policy": version, "flags": map[string]bool{"enabled": true}}`)
	a := mustLiteral(t, r, first, nil)
	b := mustLiteral(t, r, second, nil)
	if reflect.DeepEqual(a, b) {
		t.Fatal("nested seed flag lost")
	}
	r.constants["version"] = &ast.BasicLit{Kind: token.INT, Value: "2"}
	c := mustLiteral(t, r, first, nil)
	if reflect.DeepEqual(a, c) {
		t.Fatal("package constant lost")
	}
	if strings.Contains(string(canonicalJSON(t, generatedSeeds(t))), "00000000-0000-0000-0000-000000000000") {
		t.Fatal("generated seed manifest includes zero UUID row fields")
	}
}

func parseSeedExpr(source string) (ast.Expr, error) { return parser.ParseExpr(source) }
