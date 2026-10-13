package registry

import (
	"context"
	"go/ast"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each unsafe type lives in its own copied fixture. The refusal is deliberately
// source-universe-wide, so an unused custom Context also blocks a clean root.
func TestClosureContextCancelRejectsModuleContextUniverse(t *testing.T) {
	const header = `package cancellation
import ("context"; "time")
var _ = context.Background
var _ = time.Time{}
func hidden() {}
`
	const valueMethods = `
func (c custom) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c custom) Done() <-chan struct{} { return c.done }
func (c custom) Err() error { return nil }
func (c custom) Value(any) any { return nil }
`
	const custom = `type custom struct { done chan struct{} }
`
	for _, test := range []struct{ name, source string }{
		{"afterfunc-hidden-stop", custom + valueMethods + `
func (c custom) AfterFunc(func()) func() bool { return func() bool { hidden(); return true } }
func UnsafeRoot() { _, cancel := context.WithCancel(custom{make(chan struct{})}); cancel() }
`},
		{"direct-method-hidden", custom + strings.Replace(valueMethods, "return c.done", "hidden(); return c.done", 1) + `
func UnsafeRoot() { _, cancel := context.WithCancel(custom{make(chan struct{})}); cancel() }
`},
		{"pointer-methods", custom + strings.ReplaceAll(valueMethods, "(c custom)", "(c *custom)") + `
func UnsafeRoot() { _, cancel := context.WithCancel(&custom{make(chan struct{})}); cancel() }
`},
		{"promoted-interface", `
type custom struct { context.Context }
func UnsafeRoot() { _, cancel := context.WithCancel(custom{context.Background()}); cancel() }
`},
		{"local-type", `
func UnsafeRoot() {
 type local struct { context.Context }
 _, cancel := context.WithCancel(local{context.Background()}); cancel()
}
`},
		{"anonymous-type", `
func UnsafeRoot() { _, cancel := context.WithCancel(struct { context.Context }{context.Background()}); cancel() }
`},
		{"generic-instance", `
type custom[T any] struct { done chan struct{} }
func (c custom[T]) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c custom[T]) Done() <-chan struct{} { return c.done }
func (c custom[T]) Err() error { return nil }
func (c custom[T]) Value(any) T { var zero T; return zero }
func UnsafeRoot() { _, cancel := context.WithCancel(custom[any]{make(chan struct{})}); cancel() }
`},
		{"generic-factory-signature", `
type custom[T any] struct { done chan struct{} }
func (c custom[T]) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c custom[T]) Done() <-chan struct{} { return c.done }
func (c custom[T]) Err() error { return nil }
func (c custom[T]) Value(any) T { var zero T; return zero }
func makeCustom[T any](_ T) custom[T] { return custom[T]{make(chan struct{})} }
func UnsafeRoot() { _, cancel := context.WithCancel(makeCustom(any(nil))); cancel() }
`},
		{"erased-generic-body", `
type custom[T any] struct { done chan struct{} }
func (c custom[T]) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c custom[T]) Done() <-chan struct{} { return c.done }
func (c custom[T]) Err() error { return nil }
func (c custom[T]) Value(any) T { hidden(); var zero T; return zero }
func use[T any](v T) { var erased any = custom[T]{make(chan struct{})}; _, cancel := context.WithCancel(erased.(context.Context)); cancel() }
func UnsafeRoot() { use[any](nil) }
`},
		{"erased-anonymous-generic-body", `
type custom[T any] struct { done chan struct{} }
func (c custom[T]) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c custom[T]) Done() <-chan struct{} { return c.done }
func (c custom[T]) Err() error { return nil }
func (c custom[T]) Value(any) T { hidden(); var zero T; return zero }
func use[T any](v T) { var erased any = struct { custom[T] }{custom[T]{make(chan struct{})}}; _, cancel := context.WithCancel(erased.(context.Context)); cancel() }
func UnsafeRoot() { use[any](nil) }
`},
		{"erased-local-generic-body", `
type custom[T any] struct { done chan struct{} }
func (c custom[T]) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c custom[T]) Done() <-chan struct{} { return c.done }
func (c custom[T]) Err() error { return nil }
func (c custom[T]) Value(any) T { hidden(); var zero T; return zero }
func use[T any](v T) { type local struct { custom[T] }; var erased any = local{custom[T]{make(chan struct{})}}; _, cancel := context.WithCancel(erased.(context.Context)); cancel() }
func UnsafeRoot() { use[any](nil) }
`},
		{"nongeneric-lookalike", `
type custom struct{}
func (custom) Deadline() int { return 0 }
func (custom) Done() int { return 0 }
func (custom) Err() int { return 0 }
func (custom) Value() int { return 0 }
func UnsafeRoot() { _, cancel := context.WithCancel(context.Background()); cancel() }
`},
		{"generic-alias-struct", `
type custom[T any] = struct { context.Context; Extra T }
func UnsafeRoot() { _, cancel := context.WithCancel(custom[int]{Context: context.Background()}); cancel() }
`},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := copyClosureFixture(t)
			if err := os.WriteFile(filepath.Join(root, "cancellation", "custom.go"), []byte(header+test.source), 0o600); err != nil {
				t.Fatal(err)
			}
			program, err := loadClosureProgram(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			stats, err := program.contextCancelSourceUniverse()
			if err == nil || len(stats.ContextTypes)+len(stats.PotentialContextTypes) == 0 {
				t.Fatalf("custom or potential Context not refused: %+v; %v", stats, err)
			}
			for _, name := range []string{"UnsafeRoot", "Deadline"} {
				if _, err := program.closure("closurefixture.test/cancellation." + name); err == nil {
					t.Fatalf("unsafe source universe accepted through %s", name)
				}
			}
		})
	}
}

// This root-owned diagnostic measures the actual complete release source type
// universe. It is not a prediction about a future checkout's implementations.
func TestClosureContextCancelProjectUniverse(t *testing.T) {
	program, err := loadClosureProgram(context.Background(), registryRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	stats, err := program.contextCancelSourceUniverse()
	t.Logf("context cancellation source universe: packages=%d files=%d indexed=%d declarations=%d queued=%d inspected=%d concrete=%d Context=%v potential_Context=%v",
		stats.Packages, stats.SourceFiles, stats.IndexedTypes, stats.TypeDeclarations, stats.QueuedTypes, stats.VisitedTypes, stats.ConcreteTypes, stats.ContextTypes, stats.PotentialContextTypes)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.ContextTypes)+len(stats.PotentialContextTypes) != 0 {
		t.Fatal("module Context types or potential shapes present")
	}
}

func TestClosureContextCancelRejectsIncompleteTypeUniverse(t *testing.T) {
	program, err := loadClosureProgram(context.Background(), copyClosureFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range program.packages {
		pkg.info.Instances = nil
		break
	}
	if _, err := program.contextCancelSourceUniverse(); err == nil {
		t.Fatal("incomplete type evidence accepted")
	}
	if _, err := program.closure("closurefixture.test/cancellation.Deadline"); err == nil {
		t.Fatal("terminal accepted with incomplete type evidence")
	}
}

// The actual checker records an Invalid type for these constant-result builtin
// identifiers. Their surrounding expressions still enter the source universe.
func TestClosureContextCancelConstantBuiltinPlaceholders(t *testing.T) {
	program, err := loadClosureProgram(context.Background(), copyClosureFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{"len": false, "cap": false}
	for _, source := range program.packages {
		for expression, value := range source.info.Types {
			identifier, ok := expression.(*ast.Ident)
			if !ok || !value.IsBuiltin() || value.Type != types.Typ[types.Invalid] {
				continue
			}
			if _, expected := found[identifier.Name]; expected {
				found[identifier.Name] = true
			}
		}
	}
	for name, present := range found {
		if !present {
			t.Fatalf("constant %s builtin Invalid placeholder was not exercised", name)
		}
	}
	for _, name := range []string{"ConstantLen", "ConstantCap"} {
		if _, err := program.closure("closurefixture.test/cancellation." + name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClosureContextCancelRejectsInvalidValueEvidence(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "direct"
		if nested {
			name = "nested"
		}
		t.Run(name, func(t *testing.T) {
			program, err := loadClosureProgram(context.Background(), copyClosureFixture(t))
			if err != nil {
				t.Fatal(err)
			}
			injected := false
			for _, source := range program.packages {
				for expression, value := range source.info.Types {
					if value.IsBuiltin() || !value.IsValue() || value.Type == nil {
						continue
					}
					// Preserve the existing non-builtin value mode and source
					// expression; corrupt only its direct or nested type evidence.
					value.Type = types.Typ[types.Invalid]
					if nested {
						value.Type = types.NewSlice(types.Typ[types.Invalid])
					}
					source.info.Types[expression] = value
					injected = true
					break
				}
				if injected {
					break
				}
			}
			if !injected {
				t.Fatal("no non-builtin value evidence to corrupt")
			}
			if _, err := program.contextCancelSourceUniverse(); err == nil || err.Error() != "closure invalid source type" {
				t.Fatalf("invalid non-builtin evidence not refused: %v", err)
			}
			if _, err := program.closure("closurefixture.test/cancellation.Deadline"); err == nil {
				t.Fatal("terminal accepted with invalid non-builtin evidence")
			}
		})
	}
}
