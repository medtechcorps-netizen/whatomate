// Package registry contains inert source contracts only. No production code
// imports it and no test in this package opens a database.
package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const modulePath = "github.com/shridarpatil/whatomate"
const databasePath = modulePath + "/internal/database."

type dataStep struct {
	ID            string   `json:"id"`
	Rev           int      `json:"rev"`
	Func          string   `json:"func"`
	Kind          string   `json:"kind"`
	Baseline      bool     `json:"baseline"`
	Idempotent    bool     `json:"idempotent"`
	N1Safe        bool     `json:"n1_safe"`
	RequiresGates []string `json:"requires_gates"`
	ClosureFiles  []string `json:"closure_files"`
	ClosureSHA256 string   `json:"closure_sha256"`
	// Parent denotes inert child metadata, never a second executable step.
	Parent string `json:"parent,omitempty"`
}

type stepContract struct {
	Name, Symbol, Kind, Parent, IdempotencyReason string
	Idempotent                                    bool
}

// Specific seed-reconciler classifications take precedence over the plan's
// broad reference to the fourteen preparation labels. This is metadata about
// existing code, not a new scheduler or a compatibility verdict.
var stepContracts = []stepContract{
	{"PrepareProviderIntegrationManagementMode", "", "baseline-pre-ledger", "", "Adds a missing column and fills only missing provider management modes", true},
	{"PrepareMessageIngestionOrder", "", "baseline-pre-ledger", "", "Checks existing shape and installs stable ingestion-order preparation", true},
	{"PrepareMetaInstagramDeletionJournalTenant", "", "baseline-pre-ledger", "", "Legacy cleanup contains row deletion; no blanket idempotency claim", false},
	{"AutoMigrate", "", "baseline-pre-ledger", "", "Full model list is source-bound; ORM-wide idempotency is not asserted by this inert registry", false},
	{"InstallMessageIngestionOrderTrigger", "", "baseline-pre-ledger", "", "Replaces the same named trigger/function", true},
	{"BackfillProviderIntegrationBindings", "", "baseline-pre-ledger", "", "Updates only NULL Threads app bindings", true},
	{"SeedPermissionsAndRoles", "", "seed-reconciler", "", "Checks resource/action identity before creating a missing permission", true},
	{"SeedSystemRolesForAllOrgs", "", "seed-reconciler", "", "Composite repair includes conditional substeps; no blanket repeat-safety claim", false},
	{"seedSystemRolesForOrg", "", "seed-reconciler", "SeedSystemRolesForAllOrgs", "Existing system-role count skips creation; partial-create recovery is not asserted", false},
	{"FixSystemRolePermissions", "", "seed-reconciler", "SeedSystemRolesForAllOrgs", "Adds only absent permitted links and preserves explicit grants", true},
	{"ApplyManagerSettingsPolicyMigration", "", "seed-reconciler", "SeedSystemRolesForAllOrgs", "Per-organization version marker prevents reapplying the retired-grant removal", true},
	{"MigrateExistingUserRoles", "", "seed-reconciler", "SeedSystemRolesForAllOrgs", "Only users lacking role_id are repaired", true},
	{"legacy-super-admin-update", "SeedSystemRolesForAllOrgs", "seed-reconciler", "SeedSystemRolesForAllOrgs", "Existing inline UPDATE sets the same boolean; no permission-policy endorsement", true},
	{"MigrateUserOrganizations", "", "baseline-pre-ledger", "", "LEFT JOIN and NULL membership predicate select only absent live memberships", true},
	{"CreateDefaultAdmin", "", "baseline-pre-ledger", "", "Only a completely new installation is admitted; partial multi-row creation is not certified", false},
	{"EnsurePlatformReseller", "", "seed-reconciler", "", "Existing membership writes are unconditional; strict repeat safety is not certified", false},
	{"EnsureReReplyProductCatalog", "", "seed-reconciler", "", "Version markers and immutable price checks preserve reviewed catalog identities", true},
	{"SeedDefaultWidgets", "", "seed-reconciler", "", "Skips organizations with widgets; partial-create completeness is not certified", false},
	{"BackfillLastInboundAt", "", "baseline-pre-ledger", "", "Updates only NULL last_inbound_at", true},
	{"BackfillChatbotFlowGraph", modulePath + "/internal/handlers.BackfillChatbotFlowGraph", "baseline-pre-ledger", "", "Legacy one-time preparer; no independent idempotency assertion here", false},
	{"BackfillLegacyWhatsAppInbox", modulePath + "/internal/channel.BackfillLegacyWhatsAppInbox", "baseline-pre-ledger", "", "Legacy backfill; no independent idempotency assertion here", false},
}

func registryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}
func symbolFor(c stepContract) string {
	if strings.Contains(c.Symbol, "/") {
		return c.Symbol
	}
	if c.Symbol != "" {
		return databasePath + c.Symbol
	}
	return databasePath + c.Name
}
func stepID(c stepContract) string {
	if c.Parent != "" {
		return c.Parent + "/" + c.Name
	}
	return c.Name
}
func digestSource(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func formatted(node ast.Node) string {
	var b bytes.Buffer
	if err := format.Node(&b, token.NewFileSet(), node); err != nil {
		panic(err)
	}
	return b.String()
}
func parseSource(t *testing.T, root, rel string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, filepath.FromSlash(rel)), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func sourceFunction(t *testing.T, f *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == name && fn.Recv == nil {
			return fn
		}
	}
	t.Fatalf("missing source function %s", name)
	return nil
}
func sourceValue(t *testing.T, f *ast.File, name string) ast.Expr {
	t.Helper()
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok {
			for _, s := range g.Specs {
				if v, ok := s.(*ast.ValueSpec); ok {
					for i, n := range v.Names {
						if n.Name == name && len(v.Values) == len(v.Names) {
							return v.Values[i]
						}
					}
				}
			}
		}
	}
	t.Fatalf("missing or unsupported source value %s", name)
	return nil
}
func strictRead(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err = dec.Decode(into); err != nil {
		t.Fatal(err)
	}
	if err = dec.Decode(new(any)); err != io.EOF {
		t.Fatalf("trailing JSON in %s", path)
	}
	if !bytes.Equal(b, canonicalJSON(t, into)) {
		t.Fatalf("non-canonical or duplicate-key JSON in %s", path)
	}
}
func canonicalJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}
func compareOrUpdate(t *testing.T, path, env string, value any) {
	t.Helper()
	want := canonicalJSON(t, value)
	if os.Getenv(env) == "1" {
		if err := os.WriteFile(path, want, 0644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs; review the source delta before explicit %s=1 regeneration", path, env)
	}
}

type coordinatorBindings struct {
	Version           int               `json:"version"`
	ExecutionLabels   []string          `json:"execution_labels"`
	ModelDescriptors  []string          `json:"model_descriptors"`
	ModelListSHA256   string            `json:"model_list_sha256"`
	CoordinatorSHA256 string            `json:"coordinator_sha256"`
	CoordinatorCalls  []string          `json:"coordinator_calls"`
	SeedChildCalls    []string          `json:"seed_child_calls"`
	SeedChildOrder    []string          `json:"seed_child_order"`
	InlineSQL         string            `json:"inline_sql"`
	Callbacks         []string          `json:"callbacks"`
	CallbackCalls     []string          `json:"callback_calls"`
	CallbackSHA256    string            `json:"callback_sha256"`
	Idempotency       map[string]string `json:"idempotency_rationale"`
	N1Safety          string            `json:"n1_safety_note"`
	ChildSemantics    string            `json:"child_semantics"`
}

func callbackInventory(callback *ast.FuncLit) ([]string, error) {
	calls := []string{}
	ast.Inspect(callback.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			calls = append(calls, formatted(c.Fun))
		}
		return true
	})
	want := []string{"handlers.BackfillChatbotFlowGraph", "fmt.Errorf", "channelapi.BackfillLegacyWhatsAppInbox", "fmt.Errorf", "lo.Info"}
	if !reflect.DeepEqual(calls, want) {
		return nil, fmt.Errorf("coordinator callback calls changed: %v", calls)
	}
	return calls, nil
}

func seedChildOrder(body *ast.BlockStmt) []string {
	order := []string{}
	ast.Inspect(body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if name, ok := c.Fun.(*ast.Ident); ok {
				order = append(order, name.Name)
			} else if formatted(c.Fun) == "db.Exec" {
				order = append(order, "legacy-super-admin-update")
			}
		}
		return true
	})
	return order
}

func deriveBindings(t *testing.T) coordinatorBindings {
	t.Helper()
	root := registryRoot(t)
	f := parseSource(t, root, "internal/database/postgres.go")
	labels := []string{}
	lit, ok := sourceValue(t, f, "rlsMigrationPreparationLabels").(*ast.CompositeLit)
	if !ok {
		t.Fatal("unsupported labels")
	}
	for _, v := range lit.Elts {
		var s string
		if err := json.Unmarshal([]byte(formatted(v)), &s); err != nil {
			t.Fatal(err)
		}
		labels = append(labels, s)
	}
	models := sourceFunction(t, f, "GetMigrationModels")
	descriptors := []string{}
	ret, ok := models.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(models.Body.List) != 1 || len(ret.Results) != 1 {
		t.Fatal("unsupported model list")
	}
	list, ok := ret.Results[0].(*ast.CompositeLit)
	if !ok {
		t.Fatal("unsupported model list")
	}
	for _, v := range list.Elts {
		row, ok := v.(*ast.CompositeLit)
		if !ok || len(row.Elts) != 2 {
			t.Fatal("unsupported model row")
		}
		var name string
		if err := json.Unmarshal([]byte(formatted(row.Elts[0])), &name); err != nil {
			t.Fatal(err)
		}
		descriptors = append(descriptors, name+"="+formatted(row.Elts[1]))
	}
	fn := sourceFunction(t, f, "runMigrationWithProgressOnSessionUsingIndexes")
	calls := []string{}
	orderedLabels := []string{}
	labelSet := map[string]bool{}
	for _, s := range labels {
		labelSet[s] = true
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			callee := formatted(c.Fun)
			calls = append(calls, callee)
			name := callee
			if name == "silentDB.AutoMigrate" {
				name = "AutoMigrate"
			}
			if labelSet[name] {
				orderedLabels = append(orderedLabels, name)
			}
		}
		return true
	})
	if !reflect.DeepEqual(labels, orderedLabels) {
		t.Fatalf("preparation labels do not match coordinator call order: %v versus %v", labels, orderedLabels)
	}
	parent := sourceFunction(t, f, "SeedSystemRolesForAllOrgs")
	child := []string{}
	inline := []string{}
	ast.Inspect(parent.Body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if name, ok := c.Fun.(*ast.Ident); ok && name.Name != "false" {
			child = append(child, name.Name)
		}
		if formatted(c.Fun) == "db.Exec" {
			if len(c.Args) != 1 {
				t.Fatal("unsupported inline SQL binding")
			}
			var s string
			if err := json.Unmarshal([]byte(formatted(c.Args[0])), &s); err != nil {
				t.Fatal(err)
			}
			inline = append(inline, s)
		}
		return true
	})
	wantChild := []string{"seedSystemRolesForOrg", "FixSystemRolePermissions", "ApplyManagerSettingsPolicyMigration", "MigrateExistingUserRoles"}
	wantChildOrder := append(append([]string{}, wantChild...), "legacy-super-admin-update")
	childOrder := seedChildOrder(parent.Body)
	if !reflect.DeepEqual(child, wantChild) || !reflect.DeepEqual(childOrder, wantChildOrder) || len(inline) != 1 || inline[0] != "UPDATE users SET is_super_admin = true WHERE email = 'admin@admin.com'" {
		t.Fatal("seed child order/inline legacy update changed; review metadata explicitly")
	}
	main := parseSource(t, root, "cmd/whatomate/main.go")
	callbacks := []string{}
	callbackCalls := []string{}
	callbackSHA := ""
	found := 0
	ast.Inspect(main, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok || formatted(c.Fun) != "database.RunRLSMigrationCoordinator" {
			return true
		}
		found++
		if len(c.Args) != 5 {
			t.Fatal("coordinator callback signature changed")
		}
		callback, ok := c.Args[3].(*ast.FuncLit)
		if !ok {
			t.Fatal("coordinator callback is no longer explicit")
		}
		callbackSHA = digestSource([]byte(formatted(callback)))
		var err error
		callbackCalls, err = callbackInventory(callback)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(callback.Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				callee := formatted(c.Fun)
				if callee == "handlers.BackfillChatbotFlowGraph" || callee == "channelapi.BackfillLegacyWhatsAppInbox" {
					callbacks = append(callbacks, callee)
				}
			}
			return true
		})
		return true
	})
	if found != 1 || !reflect.DeepEqual(callbacks, []string{"handlers.BackfillChatbotFlowGraph", "channelapi.BackfillLegacyWhatsAppInbox"}) {
		t.Fatal("migration callback binding changed")
	}
	rationale := map[string]string{}
	for _, c := range stepContracts {
		rationale[stepID(c)] = c.IdempotencyReason
	}
	return coordinatorBindings{
		Version: 1, ExecutionLabels: labels, ModelDescriptors: descriptors,
		ModelListSHA256:   digestSource([]byte(formatted(models))),
		CoordinatorSHA256: digestSource([]byte(formatted(fn))),
		CoordinatorCalls:  calls, SeedChildCalls: child, SeedChildOrder: childOrder, InlineSQL: inline[0],
		Callbacks: callbacks, CallbackCalls: callbackCalls, CallbackSHA256: callbackSHA,
		Idempotency:    rationale,
		N1Safety:       "false means not certified by N-1 compatibility proof; it does not assert a demonstrated incompatibility",
		ChildSemantics: "parent entries execute conceptually once; children are inert source metadata, never additional executable steps",
	}
}

func TestCoordinatorBindingRejectsOmittedCallbacksAndMovedSQL(t *testing.T) {
	valid := `func() {
handlers.BackfillChatbotFlowGraph(session, lo)
fmt.Errorf("first")
channelapi.BackfillLegacyWhatsAppInbox(session, 500)
fmt.Errorf("second")
lo.Info("done")
}`
	for _, source := range []string{valid, strings.Replace(valid, `lo.Info("done")`, `newDataBackfill(session); lo.Info("done")`, 1)} {
		expr, err := parser.ParseExpr(source)
		if err != nil {
			t.Fatal(err)
		}
		_, err = callbackInventory(expr.(*ast.FuncLit))
		if (source == valid) != (err == nil) {
			t.Fatal("unexpected callback acceptance")
		}
	}
	before, err := parser.ParseExpr(`func() { first(db); second(db); db.Exec("fixed SQL") }`)
	if err != nil {
		t.Fatal(err)
	}
	after, err := parser.ParseExpr(`func() { first(db); db.Exec("fixed SQL"); second(db) }`)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(seedChildOrder(before.(*ast.FuncLit).Body), seedChildOrder(after.(*ast.FuncLit).Body)) {
		t.Fatal("moving inline SQL was lost from child order")
	}
}

func TestDataStepRegistryMatchesCoordinator(t *testing.T) {
	bindings := deriveBindings(t)
	compareOrUpdate(t, "coordinator_bindings.json", "REREPLY_UPDATE_REGISTRY", bindings)
	// Explicit regeneration must not depend on the order in which Go chooses
	// this test and TestDataStepClosureHashes (including -shuffle runs).
	if os.Getenv("REREPLY_UPDATE_REGISTRY") == "1" {
		compareOrUpdate(t, "data_steps.json", "REREPLY_UPDATE_REGISTRY", generatedSteps(t))
	}
	var entries []dataStep
	strictRead(t, "data_steps.json", &entries)
	if len(entries) != len(stepContracts) {
		t.Fatalf("registry step count %d != source contract %d", len(entries), len(stepContracts))
	}
	seen := map[string]bool{}
	execution := []string{}
	for i, c := range stepContracts {
		e := entries[i]
		if seen[e.ID] {
			t.Fatalf("duplicate step %s", e.ID)
		}
		seen[e.ID] = true
		if e.ID != stepID(c) || e.Func != symbolFor(c) || e.Rev != 1 || e.Kind != c.Kind || !e.Baseline || e.Parent != c.Parent || e.Idempotent != c.Idempotent || e.N1Safe || e.RequiresGates == nil || len(e.RequiresGates) != 0 {
			t.Fatalf("entry %d metadata does not match reviewed source contract", i)
		}
		if e.Parent == "" && !strings.HasPrefix(e.Func, modulePath+"/internal/handlers.") && !strings.HasPrefix(e.Func, modulePath+"/internal/channel.") {
			execution = append(execution, c.Name)
		}
	}
	if !reflect.DeepEqual(execution, bindings.ExecutionLabels) {
		t.Fatalf("omitted or duplicated coordinator execution labels: %v", execution)
	}
}

var loadedClosure struct {
	sync.Once
	program *closureProgram
	err     error
}

func registryProgram(t *testing.T) *closureProgram {
	t.Helper()
	loadedClosure.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		loadedClosure.program, loadedClosure.err = loadClosureProgram(ctx, registryRoot(t))
	})
	if loadedClosure.err != nil {
		t.Fatal(loadedClosure.err)
	}
	return loadedClosure.program
}
func generatedSteps(t *testing.T) []dataStep {
	t.Helper()
	p := registryProgram(t)
	steps := []dataStep{}
	for _, c := range stepContracts {
		// GORM reaches these naming/lifecycle methods reflectively. The explicit
		// pinned interface contract includes only reached model receiver types;
		// unrelated methods (including String) stay outside the closure.
		result, err := p.closureWithGORMModels(modulePath+"/internal/models", symbolFor(c))
		if err != nil {
			t.Fatal(err)
		}
		steps = append(steps, dataStep{stepID(c), 1, symbolFor(c), c.Kind, true, c.Idempotent, false, []string{}, result.Files, result.SHA256, c.Parent})
	}
	return steps
}
func TestDataStepClosureHashes(t *testing.T) {
	capsBefore, err := os.ReadFile("closure_caps.json")
	if err != nil {
		t.Fatal(err)
	}
	steps := generatedSteps(t)
	compareOrUpdate(t, "data_steps.json", "REREPLY_UPDATE_REGISTRY", steps)
	capsAfter, err := os.ReadFile("closure_caps.json")
	if err != nil || !bytes.Equal(capsBefore, capsAfter) {
		t.Fatal("registry regeneration changed separately reviewed caps")
	}
}

func TestDataStepClosureCaps(t *testing.T) {
	// Caps are separately reviewed. UPDATE_REGISTRY never creates or raises
	// them; an added source file requires an explicit reviewed cap change.
	// Counts include reached-package import declarations and initialization.
	// A new import can therefore require review without adding a called body.
	var caps map[string]int
	strictRead(t, "closure_caps.json", &caps)
	steps := generatedSteps(t)
	if len(caps) != len(steps) {
		t.Fatal("closure cap inventory differs")
	}
	for _, e := range steps {
		cap, ok := caps[e.ID]
		if !ok || cap < 1 {
			t.Fatalf("missing cap: %s", e.ID)
		}
		if err := requireClosureCap(closureResult{Files: e.ClosureFiles}, cap); err != nil {
			t.Fatalf("%s: %v", e.ID, err)
		}
		t.Logf("%s: %d/%d files", e.ID, len(e.ClosureFiles), cap)
	}
}

func TestRegistryUpdateNeverRaisesCaps(t *testing.T) {
	t.Setenv("REREPLY_UPDATE_REGISTRY", "1")
	dir := t.TempDir()
	path := filepath.Join(dir, "closure_caps.json")
	original := canonicalJSON(t, map[string]int{"synthetic": 1})
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	// The actual generation writer updates only the requested registry file.
	// The independent cap check still refuses even with the update opt-in.
	result := closureResult{Files: []string{"a.go", "b.go"}}
	compareOrUpdate(t, filepath.Join(dir, "data_steps.json"), "REREPLY_UPDATE_REGISTRY", result.Files)
	var caps map[string]int
	strictRead(t, path, &caps)
	err := requireClosureCap(result, caps["synthetic"])
	if err == nil || !strings.Contains(err.Error(), "a.go\nb.go") {
		t.Fatal("update opt-in bypassed cap refusal or omitted reached-file list")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("update opt-in raised caps")
	}
}

func TestDataStepRegistryShape(t *testing.T) {
	// The canonical generated bytes, strict struct decoder and source order
	// bind metadata; this assertion also keeps all closure paths portable.
	var entries []dataStep
	strictRead(t, "data_steps.json", &entries)
	for _, e := range entries {
		if e.Kind != "seed-reconciler" && e.Kind != "baseline-pre-ledger" {
			t.Fatal("unexpected executable ledger entry before PR22")
		}
		if len(e.ClosureSHA256) != 64 {
			t.Fatal("invalid closure hash")
		}
		if _, err := hex.DecodeString(e.ClosureSHA256); err != nil {
			t.Fatal(err)
		}
		if !sort.StringsAreSorted(e.ClosureFiles) {
			t.Fatal("unsorted files")
		}
		for _, f := range e.ClosureFiles {
			if filepath.IsAbs(f) || strings.Contains(f, "\\") || strings.HasPrefix(f, "../") || strings.HasSuffix(f, "_test.go") {
				t.Fatalf("non-production closure path %s", f)
			}
		}
	}
}

func TestRegistryClosurePathsAreProductionSources(t *testing.T) {
	// Evidence is recorded by the author with git diff at review time. This
	// test intentionally makes no Git-history or production-compatibility claim.
	var entries []dataStep
	strictRead(t, "data_steps.json", &entries)
	for _, e := range entries {
		for _, f := range e.ClosureFiles {
			if strings.HasPrefix(f, "internal/dbcatalog/registry/") {
				t.Fatal("registry must not enter production closures")
			}
			if _, err := os.Stat(filepath.Join(registryRoot(t), filepath.FromSlash(f))); err != nil {
				t.Fatal(err)
			}
		}
	}
}
