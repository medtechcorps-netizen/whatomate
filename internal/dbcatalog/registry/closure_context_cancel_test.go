package registry

import (
	"go/ast"
	"go/token"
	"go/types"
)

// contextCancelCall admits a terminal only for the exact standard-library
// context.CancelFunc returned by one proven local tuple initializer. This is
// deliberately not part of valueCallable: cancellation values may not acquire
// aliases, escape as callbacks, or become a general external-function waiver.
func (p *closureProgram) contextCancelCall(decl *closureDeclaration, call *ast.CallExpr) bool {
	ident, ok := closureUnparen(call.Fun).(*ast.Ident)
	if !ok || len(call.Args) != 0 || call.Ellipsis.IsValid() {
		return false
	}
	local, ok := decl.pkg.info.Uses[ident].(*types.Var)
	if !ok || p.objects[local] != nil {
		return false
	}
	cancel, ok := types.Unalias(local.Type()).(*types.Named)
	if !ok || cancel.Obj().Pkg() == nil || cancel.Obj().Pkg().Path() != "context" || cancel.Obj().Name() != "CancelFunc" {
		return false
	}
	contextPackage := cancel.Obj().Pkg()
	if p.packages["context"] != nil || contextPackage.Scope().Lookup("CancelFunc") != cancel.Obj() {
		return false
	}
	// The standard named type is func() with no result, not merely an
	// arbitrary function type convertible to that signature.
	signature, ok := cancel.Underlying().(*types.Signature)
	if !ok || signature.Params().Len() != 0 || signature.Results().Len() != 0 || signature.Variadic() {
		return false
	}
	if _, ok := decl.node.(*ast.FuncDecl); !ok {
		return false
	}

	var initializer *ast.CallExpr
	definitions := 0
	allowedUses := map[*ast.Ident]bool{}
	ast.Inspect(decl.node, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.Ident:
			if decl.pkg.info.Defs[node] == local {
				definitions++
			}
		case *ast.AssignStmt:
			if node.Tok == token.DEFINE && len(node.Lhs) == 2 && len(node.Rhs) == 1 {
				first, firstOK := node.Lhs[0].(*ast.Ident)
				second, secondOK := node.Lhs[1].(*ast.Ident)
				if firstOK && first != nil && secondOK && decl.pkg.info.Defs[second] == local {
					initializer, _ = node.Rhs[0].(*ast.CallExpr)
				}
			}
		case *ast.ValueSpec:
			if len(node.Names) == 2 && len(node.Values) == 1 && decl.pkg.info.Defs[node.Names[1]] == local {
				initializer, _ = node.Values[0].(*ast.CallExpr)
			}
		case *ast.CallExpr:
			if name, ok := closureUnparen(node.Fun).(*ast.Ident); ok && decl.pkg.info.Uses[name] == local && len(node.Args) == 0 && !node.Ellipsis.IsValid() {
				allowedUses[name] = true
			}
		}
		return true // Include nested literals: they share the captured cell.
	})
	if definitions != 1 || initializer == nil || !p.contextCancelInitializer(decl, initializer, contextPackage) {
		return false
	}
	immutable := true
	ast.Inspect(decl.node, func(node ast.Node) bool {
		if name, ok := node.(*ast.Ident); ok && decl.pkg.info.Uses[name] == local && !allowedUses[name] {
			// Assignment, address-taking, aliases, returning/storing/passing the
			// value, and all other non-call references fail closed.
			immutable = false
		}
		return immutable
	})
	return immutable
}

// A direct function or concrete-receiver method is statically selected. Local
// function variables, fields, method expressions and interface dispatch are not
// wrapper proofs, regardless of their spelling or result type.
func closureDirectFunction(info *types.Info, call *ast.CallExpr) *types.Func {
	switch fun := closureUnparen(call.Fun).(type) {
	case *ast.Ident:
		fn, _ := info.Uses[fun].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		if selected := info.Selections[fun]; selected != nil {
			if selected.Kind() != types.MethodVal {
				return nil
			}
			receiver := types.Unalias(selected.Recv())
			if pointer, ok := receiver.(*types.Pointer); ok {
				receiver = types.Unalias(pointer.Elem())
			}
			if _, dynamic := receiver.Underlying().(*types.Interface); dynamic {
				return nil
			}
			fn, _ := selected.Obj().(*types.Func)
			return fn
		}
		fn, _ := info.Uses[fun.Sel].(*types.Func)
		return fn
	}
	return nil
}

func closureContextFactory(info *types.Info, call *ast.CallExpr, contextPackage *types.Package) bool {
	fn := closureDirectFunction(info, call)
	if fn == nil || fn.Pkg() != contextPackage || contextPackage.Scope().Lookup(fn.Name()) != fn {
		return false
	}
	switch fn.Name() {
	case "WithCancel", "WithDeadline", "WithTimeout":
		return true
	}
	return false
}

func (p *closureProgram) contextCancelInitializer(decl *closureDeclaration, call *ast.CallExpr, contextPackage *types.Package) bool {
	if closureContextFactory(decl.pkg.info, call, contextPackage) {
		return true
	}
	fn := closureDirectFunction(decl.pkg.info, call)
	if fn == nil {
		return false
	}
	wrapper := p.objects[fn.Origin()]
	if wrapper == nil {
		return false
	}
	body, ok := wrapper.node.(*ast.FuncDecl)
	if !ok || body.Body == nil {
		return false
	}
	sig, ok := fn.Type().(*types.Signature)
	contextType := contextPackage.Scope().Lookup("Context")
	cancelType := contextPackage.Scope().Lookup("CancelFunc")
	if !ok || contextType == nil || cancelType == nil || sig.Results().Len() != 2 ||
		sig.Results().At(0).Name() != "" || sig.Results().At(1).Name() != "" ||
		!types.Identical(sig.Results().At(0).Type(), contextType.Type()) ||
		!types.Identical(sig.Results().At(1).Type(), cancelType.Type()) {
		return false
	}
	// Permit one hop only, and one explicit tuple return directly from the
	// factory. Named results, alternative returns and nested literals are
	// deliberately unsupported; a deferred write must not replace a result.
	returns := 0
	valid := true
	ast.Inspect(body.Body, func(node ast.Node) bool {
		if _, nested := node.(*ast.FuncLit); nested {
			valid = false
			return false
		}
		if statement, ok := node.(*ast.ReturnStmt); ok {
			returns++
			if len(statement.Results) != 1 {
				valid = false
				return false
			}
			factory, ok := statement.Results[0].(*ast.CallExpr)
			if !ok || !closureContextFactory(wrapper.pkg.info, factory, contextPackage) {
				valid = false
			}
		}
		return true
	})
	return valid && returns == 1
}
