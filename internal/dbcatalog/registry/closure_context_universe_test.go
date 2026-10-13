package registry

import (
	"errors"
	"go/types"
	"sort"
)

// A standard CancelFunc is not module-free if an opaque parent can implement
// Context in this source universe. In particular, context may call its methods
// and an AfterFunc-returned stop closure. Refuse that whole case, rather than
// guessing the runtime parent or attempting another callback-dispatch model.
type closureContextUniverse struct {
	Packages, SourceFiles, IndexedTypes, TypeDeclarations int
	QueuedTypes, VisitedTypes, ConcreteTypes              int
	ContextTypes, PotentialContextTypes                   []string
}

func (p *closureProgram) contextCancelSourceUniverse() (closureContextUniverse, error) {
	stats := closureContextUniverse{IndexedTypes: len(p.types), TypeDeclarations: len(p.typeDeclarations)}
	fail := func(message string) (closureContextUniverse, error) { return stats, errors.New(message) }
	// Import uses the same checked/export type identity as the rest of this
	// program; main-module shadowing of the standard package is forbidden.
	if p.exports == nil {
		return fail("closure context export unavailable")
	}
	if p.packages["context"] != nil {
		return fail("closure nonstandard context package")
	}
	pkg, err := p.Import("context")
	if err != nil || pkg == nil || pkg.Path() != "context" {
		return fail("closure context type unavailable")
	}
	contextName, ok := pkg.Scope().Lookup("Context").(*types.TypeName)
	if !ok {
		return fail("closure context type unavailable")
	}
	contextInterface, ok := contextName.Type().Underlying().(*types.Interface)
	if !ok {
		return fail("closure context interface unavailable")
	}
	contextInterface.Complete()

	// This is a refusal bound, not a source/call cap expansion. Count queued
	// distinct types as well as visited types, so the work list stays bounded.
	// Measured complete project universe: 65,711 types; retain a finite ceiling.
	const maximumTypes = 100000
	queue := []types.Type{}
	seen := map[types.Type]bool{}
	var walkErr error
	add := func(typ types.Type) {
		if walkErr != nil {
			return
		}
		if typ == nil {
			walkErr = errors.New("closure missing type evidence")
			return
		}
		typ = types.Unalias(typ)
		if typ == nil {
			walkErr = errors.New("closure incomplete alias evidence")
			return
		}
		if seen[typ] {
			return
		}
		if len(seen) >= maximumTypes {
			walkErr = errors.New("closure context type universe limit")
			return
		}
		seen[typ] = true
		queue = append(queue, typ)
		stats.QueuedTypes = len(seen)
	}
	addObject := func(object types.Object) {
		if object == nil {
			return
		}
		switch object.(type) {
		case *types.PkgName, *types.Builtin, *types.Label:
			// Import aliases, builtins and labels do not declare concrete types.
			return
		default:
			add(object.Type())
		}
	}
	for _, typ := range p.types {
		add(typ)
	}
	for typ := range p.typeDeclarations {
		add(typ)
	}
	for _, source := range p.packages {
		if source == nil || source.loading || source.typed == nil || source.info == nil || len(source.files) == 0 ||
			source.info.Types == nil || source.info.Defs == nil || source.info.Uses == nil || source.info.Instances == nil {
			return fail("closure incomplete source type universe")
		}
		stats.Packages++
		stats.SourceFiles += len(source.files)
		for _, value := range source.info.Types {
			// Builtin identifiers have synthetic call-site types; for constant
			// results go/types intentionally records Invalid (Info.Types docs).
			// Arguments, type expressions and call results remain separately
			// seeded. Custom shadows are ordinary values, not IsBuiltin.
			if value.Type != nil && !value.IsBuiltin() {
				add(value.Type)
			}
		}
		for _, object := range source.info.Defs {
			addObject(object)
		}
		for _, object := range source.info.Uses {
			addObject(object)
		}
		for _, instance := range source.info.Instances {
			add(instance.Type)
		}
	}
	if stats.Packages == 0 || stats.SourceFiles == 0 {
		return fail("closure empty source type universe")
	}

	for len(queue) != 0 && walkErr == nil {
		typ := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		stats.VisitedTypes++
		candidate := false
		switch value := typ.(type) {
		case *types.Named:
			obj := value.Obj()
			if obj == nil {
				walkErr = errors.New("closure missing named-type identity")
				break
			}
			if obj.Pkg() == nil {
				// Only the actual predeclared Universe types have no package.
				// In particular, error is a Named interface, not missing evidence.
				if obj != types.Universe.Lookup("error") && obj != types.Universe.Lookup("comparable") {
					walkErr = errors.New("closure missing named-type identity")
				} else {
					add(value.Underlying())
				}
				break
			}
			for i := 0; i < value.TypeArgs().Len(); i++ {
				add(value.TypeArgs().At(i))
			}
			if p.packages[obj.Pkg().Path()] == nil {
				// Imported implementations retain the resolver's existing export
				// boundary. Do not mistake stdlib concrete types for module code.
				break
			}
			_, isInterface := value.Underlying().(*types.Interface)
			candidate = !isInterface
			add(value.Underlying())
			for i := 0; i < value.NumMethods(); i++ {
				add(value.Method(i).Type())
			}
			for i := 0; i < value.TypeParams().Len(); i++ {
				add(value.TypeParams().At(i))
			}
		case *types.Pointer:
			add(value.Elem()) // The element's value and pointer method sets are checked together.
		case *types.Struct:
			candidate = true // Includes local/anonymous/promoted and generic-alias structs.
			for i := 0; i < value.NumFields(); i++ {
				add(value.Field(i).Type())
			}
		case *types.Signature:
			if value.Recv() != nil {
				add(value.Recv().Type())
			}
			add(value.Params())
			add(value.Results())
			for i := 0; i < value.TypeParams().Len(); i++ {
				add(value.TypeParams().At(i))
			}
			for i := 0; i < value.RecvTypeParams().Len(); i++ {
				add(value.RecvTypeParams().At(i))
			}
		case *types.Tuple:
			for i := 0; i < value.Len(); i++ {
				add(value.At(i).Type())
			}
		case *types.Interface:
			value.Complete()
			for i := 0; i < value.NumExplicitMethods(); i++ {
				add(value.ExplicitMethod(i).Type())
			}
			for i := 0; i < value.NumEmbeddeds(); i++ {
				add(value.EmbeddedType(i))
			}
		case *types.TypeParam:
			add(value.Constraint())
		case *types.Union:
			for i := 0; i < value.Len(); i++ {
				add(value.Term(i).Type())
			}
		case *types.Array:
			add(value.Elem())
		case *types.Slice:
			add(value.Elem())
		case *types.Map:
			add(value.Key())
			add(value.Elem())
		case *types.Chan:
			add(value.Elem())
		case *types.Basic:
			if value.Kind() == types.Invalid {
				walkErr = errors.New("closure invalid source type")
			}
		default:
			walkErr = errors.New("closure unsupported source type evidence")
		}
		if candidate {
			stats.ConcreteTypes++
			name := types.TypeString(typ, func(pkg *types.Package) string { return pkg.Path() })
			if types.Implements(typ, contextInterface) || types.Implements(types.NewPointer(typ), contextInterface) {
				stats.ContextTypes = append(stats.ContextTypes, name)
			}
			// A generic body may erase custom[T] behind any, while only its
			// instantiated function signature is in Info.Instances. Refuse the
			// four-name shape without trying to specialize such bodies. This
			// deliberately also refuses incompatible nongeneric lookalikes.
			for _, receiver := range []types.Type{typ, types.NewPointer(typ)} {
				methods := types.NewMethodSet(receiver)
				if methods.Lookup(nil, "Deadline") != nil && methods.Lookup(nil, "Done") != nil &&
					methods.Lookup(nil, "Err") != nil && methods.Lookup(nil, "Value") != nil {
					stats.PotentialContextTypes = append(stats.PotentialContextTypes, name)
					break
				}
			}
		}
	}
	sort.Strings(stats.ContextTypes)
	sort.Strings(stats.PotentialContextTypes)
	if walkErr != nil {
		return stats, walkErr
	}
	if len(stats.ContextTypes) != 0 || len(stats.PotentialContextTypes) != 0 {
		return fail("closure module Context implementation or four-method shape prevents cancellation terminal proof")
	}
	return stats, nil
}
