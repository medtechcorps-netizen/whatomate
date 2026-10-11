package registry

// This walker is test-only. It never invokes a registered function. Export data
// describes dependencies; source declarations inside this module form the hash.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type closureResult struct {
	Files        []string
	SHA256       string
	Declarations []string
}

type closureListPackage struct {
	ImportPath, Dir, Export string
	GoFiles, CgoFiles       []string
	Module                  *struct {
		Path, Dir, GoVersion string
		Main                 bool
	}
	Error *struct{ Err string }
}

type closurePackage struct {
	meta         closureListPackage
	files        []*ast.File
	info         *types.Info
	typed        *types.Package
	loading      bool
	initializers []*closureDeclaration
}

type closureDeclaration struct {
	id, file, source string
	node             ast.Node
	pkg              *closurePackage
	imports          map[string]*closureDeclaration
}

type closureCallable struct {
	decl     *closureDeclaration
	node     ast.Node
	sig      *types.Signature
	captured map[*types.Var]*closureCallable
	key      string
}

type closureVisit struct {
	callable *closureCallable
	bindings map[*types.Var]*closureCallable
}

func closureBindingsKey(bindings map[*types.Var]*closureCallable) string {
	keys := make([]string, 0, len(bindings))
	for param, value := range bindings {
		keys = append(keys, fmt.Sprintf("%d=%s", param.Pos(), value.key))
	}
	sort.Strings(keys)
	return strings.Join(keys, ";")
}

func copyClosureBindings(bindings map[*types.Var]*closureCallable) map[*types.Var]*closureCallable {
	result := make(map[*types.Var]*closureCallable, len(bindings))
	for param, value := range bindings {
		result[param] = value
	}
	return result
}

// Callback targets are only valid while their parameter cells remain immutable.
// Scan nested literals too: a captured assignment/address escape invalidates the
// enclosing binding even if its eventual execution order is unknown.
func closureBindingsImmutable(node ast.Node, info *types.Info, bindings map[*types.Var]*closureCallable) bool {
	immutable := true
	bound := func(expr ast.Expr) bool {
		for {
			paren, ok := expr.(*ast.ParenExpr)
			if !ok {
				break
			}
			expr = paren.X
		}
		ident, ok := expr.(*ast.Ident)
		if !ok {
			return false
		}
		param, _ := info.Uses[ident].(*types.Var)
		return bindings[param] != nil
	}
	ast.Inspect(node, func(node ast.Node) bool {
		if !immutable {
			return false
		}
		switch node := node.(type) {
		case *ast.AssignStmt:
			for _, lhs := range node.Lhs {
				if bound(lhs) {
					immutable = false
				}
			}
		case *ast.RangeStmt:
			if bound(node.Key) || bound(node.Value) {
				immutable = false
			}
		case *ast.UnaryExpr:
			if node.Op == token.AND && bound(node.X) {
				immutable = false
			}
		}
		return immutable
	})
	return immutable
}

func closureUnparen(expr ast.Expr) ast.Expr {
	for {
		switch value := expr.(type) {
		case *ast.ParenExpr:
			expr = value.X
		case *ast.IndexExpr:
			expr = value.X
		case *ast.IndexListExpr:
			expr = value.X
		default:
			return expr
		}
	}
}

func (p *closureProgram) functionCallable(fn *types.Func) *closureCallable {
	fn = fn.Origin()
	if decl := p.objects[fn]; decl != nil {
		return &closureCallable{decl: decl, node: decl.node, sig: fn.Type().(*types.Signature), key: decl.id}
	}
	return nil
}

func (p *closureProgram) valueCallable(decl *closureDeclaration, expr ast.Expr, bindings map[*types.Var]*closureCallable) (*closureCallable, error) {
	expr = closureUnparen(expr)
	if literal, ok := expr.(*ast.FuncLit); ok {
		key := fmt.Sprintf("%s#literal:%d{%s}", decl.id, literal.Pos(), closureBindingsKey(bindings))
		if len(key) > 1<<16 {
			return nil, errors.New("closure callback nesting limit")
		}
		return &closureCallable{decl: decl, node: literal, sig: decl.pkg.info.Types[literal].Type.(*types.Signature), captured: copyClosureBindings(bindings), key: key}, nil
	}
	var obj types.Object
	switch expr := expr.(type) {
	case *ast.Ident:
		obj = decl.pkg.info.Uses[expr]
	case *ast.SelectorExpr:
		if selection := decl.pkg.info.Selections[expr]; selection != nil {
			// Method values need receiver capture; ordinary direct method calls
			// are handled separately. Never certify an unbound dynamic receiver.
			return nil, errors.New("closure method-valued callback is unsupported")
		}
		obj = decl.pkg.info.Uses[expr.Sel]
	}
	if param, ok := obj.(*types.Var); ok && bindings[param] != nil {
		return bindings[param], nil
	}
	if local, ok := obj.(*types.Var); ok && p.objects[local] == nil {
		// Only a local's one literal initializer is supported. No dataflow or
		// package-function-variable assumptions are made, even for aliases.
		var initializer *ast.FuncLit
		ast.Inspect(decl.node, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.AssignStmt:
				if node.Tok == token.DEFINE && len(node.Lhs) == len(node.Rhs) {
					for i, lhs := range node.Lhs {
						if ident, ok := lhs.(*ast.Ident); ok && decl.pkg.info.Defs[ident] == local {
							initializer, _ = node.Rhs[i].(*ast.FuncLit)
						}
					}
				}
			case *ast.ValueSpec:
				if len(node.Names) == len(node.Values) {
					for i, ident := range node.Names {
						if decl.pkg.info.Defs[ident] == local {
							initializer, _ = node.Values[i].(*ast.FuncLit)
						}
					}
				}
			}
			return true
		})
		if initializer != nil {
			if !closureBindingsImmutable(decl.node, decl.pkg.info, map[*types.Var]*closureCallable{local: {}}) {
				return nil, errors.New("closure local callback reassignment or address escape")
			}
			return p.valueCallable(decl, initializer, bindings)
		}
	}
	if fn, ok := obj.(*types.Func); ok {
		if callable := p.functionCallable(fn); callable != nil {
			return callable, nil
		}
		return nil, errors.New("closure external callback implementation is unavailable")
	}
	return nil, errors.New("closure unresolved function-valued call")
}

func (p *closureProgram) bindClosureCall(caller *closureDeclaration, target *closureCallable, args []ast.Expr, bindings map[*types.Var]*closureCallable) (closureVisit, error) {
	bound := copyClosureBindings(target.captured)
	for index := 0; index < target.sig.Params().Len(); index++ {
		param := target.sig.Params().At(index)
		if _, function := param.Type().Underlying().(*types.Signature); !function {
			continue
		}
		if index >= len(args) || target.sig.Variadic() && index == target.sig.Params().Len()-1 {
			return closureVisit{}, errors.New("closure unsupported callback argument shape")
		}
		value, err := p.valueCallable(caller, args[index], bindings)
		if err != nil {
			return closureVisit{}, err
		}
		bound[param] = value
	}
	return closureVisit{target, bound}, nil
}

type closureProgram struct {
	root             string
	fset             *token.FileSet
	packages         map[string]*closurePackage
	exports          types.Importer
	declarations     map[string]*closureDeclaration
	objects          map[types.Object]*closureDeclaration
	typeDeclarations map[types.Type]*closureDeclaration
	types            []types.Type
}

type closureBoundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *closureBoundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("closure go-list output limit")
	}
	return b.Buffer.Write(p)
}

// loadClosureProgram resolves one fixed release build (linux/amd64, cgo off).
// Test files and other modules are excluded. External packages use gc export
// data, while all main-module packages are checked together from source so
// receiver/interface identity is shared across package boundaries.
func loadClosureProgram(parent context.Context, repoDir string) (*closureProgram, error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	root, err := filepath.Abs(repoDir)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "go", "list", "-mod=readonly", "-deps", "-export", "-json", "./...")
	cmd.Dir = root
	cmd.Env = closureBuildEnvironment(os.Environ())
	stdout := &closureBoundedBuffer{limit: 64 << 20}
	stderr := &closureBoundedBuffer{limit: 1 << 20}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		// Tool errors can contain private environment/proxy URLs. Never echo them.
		return nil, errors.New("closure go-list export failed")
	}
	p := &closureProgram{root: root, fset: token.NewFileSet(), packages: map[string]*closurePackage{}, declarations: map[string]*closureDeclaration{}, objects: map[types.Object]*closureDeclaration{}, typeDeclarations: map[types.Type]*closureDeclaration{}}
	exportFiles := map[string]string{}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	for count := 0; ; count++ {
		var listed closureListPackage
		if err := decoder.Decode(&listed); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, errors.New("closure invalid go-list export data")
		}
		if count >= 4096 || listed.Error != nil || listed.ImportPath == "" {
			return nil, errors.New("closure invalid package inventory")
		}
		if listed.Export != "" {
			exportFiles[listed.ImportPath] = listed.Export
		}
		if listed.Module != nil && listed.Module.Main && len(listed.GoFiles) != 0 {
			if len(listed.CgoFiles) != 0 || p.packages[listed.ImportPath] != nil {
				return nil, errors.New("closure unsupported or duplicate module package")
			}
			if filepath.Clean(listed.Module.Dir) != filepath.Clean(root) {
				return nil, errors.New("closure module root mismatch")
			}
			p.packages[listed.ImportPath] = &closurePackage{meta: listed}
		}
	}
	if len(p.packages) == 0 {
		return nil, errors.New("closure no module source packages")
	}
	p.exports = importer.ForCompiler(p.fset, "gc", func(path string) (io.ReadCloser, error) {
		name := exportFiles[path]
		if name == "" {
			return nil, errors.New("closure missing dependency export")
		}
		return os.Open(name)
	})
	paths := make([]string, 0, len(p.packages))
	for path := range p.packages {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	total := 0
	for _, path := range paths {
		pkg := p.packages[path]
		sort.Strings(pkg.meta.GoFiles)
		for _, file := range pkg.meta.GoFiles {
			name := filepath.Join(pkg.meta.Dir, file)
			rel, err := filepath.Rel(root, name)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) || strings.HasSuffix(file, "_test.go") {
				return nil, errors.New("closure source outside module")
			}
			data, err := os.ReadFile(name)
			if err != nil {
				return nil, err
			}
			total += len(data)
			if len(data) > 8<<20 || total > 128<<20 {
				return nil, errors.New("closure source size limit")
			}
			parsed, err := parser.ParseFile(p.fset, name, data, parser.ParseComments|parser.AllErrors)
			if err != nil {
				return nil, errors.New("closure module parse failed")
			}
			pkg.files = append(pkg.files, parsed)
		}
	}
	for _, path := range paths {
		if _, err := p.Import(path); err != nil {
			return nil, err
		}
	}
	for _, path := range paths {
		if err := p.indexPackage(p.packages[path]); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func closureBuildEnvironment(env []string) []string {
	out := make([]string, 0, len(env)+5)
	for _, value := range env {
		key, _, _ := strings.Cut(value, "=")
		switch strings.ToUpper(key) {
		case "GOOS", "GOARCH", "CGO_ENABLED", "GOWORK", "GOFLAGS":
			continue
		}
		out = append(out, value)
	}
	return append(out, "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=")
}

func (p *closureProgram) Import(path string) (*types.Package, error) {
	pkg := p.packages[path]
	if pkg == nil {
		return p.exports.Import(path)
	}
	if pkg.typed != nil {
		return pkg.typed, nil
	}
	if pkg.loading {
		return nil, errors.New("closure import cycle")
	}
	pkg.loading = true
	defer func() { pkg.loading = false }()
	pkg.info = &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Implicits: map[ast.Node]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}, Instances: map[*ast.Ident]types.Instance{}}
	config := types.Config{Importer: p, Sizes: types.SizesFor("gc", "amd64")}
	checked, err := config.Check(path, p.fset, pkg.files, pkg.info)
	if err != nil {
		return nil, fmt.Errorf("closure module type-check failed: %s", path)
	}
	pkg.typed = checked
	return checked, nil
}

func closureObjectKey(obj types.Object) string {
	if obj == nil || obj.Pkg() == nil {
		return ""
	}
	if fn, ok := obj.(*types.Func); ok {
		fn = fn.Origin()
		if recv := fn.Type().(*types.Signature).Recv(); recv != nil {
			typ := types.Unalias(recv.Type())
			if pointer, ok := typ.(*types.Pointer); ok {
				typ = types.Unalias(pointer.Elem())
			}
			if named, ok := typ.(*types.Named); ok {
				return fn.Pkg().Path() + "." + named.Origin().Obj().Name() + "." + fn.Name()
			}
		}
	}
	return obj.Pkg().Path() + "." + obj.Name()
}

func (p *closureProgram) indexPackage(pkg *closurePackage) error {
	for _, name := range pkg.typed.Scope().Names() {
		if obj, ok := pkg.typed.Scope().Lookup(name).(*types.TypeName); ok && !obj.IsAlias() {
			if _, isInterface := obj.Type().Underlying().(*types.Interface); !isInterface {
				p.types = append(p.types, obj.Type(), types.NewPointer(obj.Type()))
			}
		}
	}
	for _, instance := range pkg.info.Instances {
		if named, ok := instance.Type.(*types.Named); ok {
			p.types = append(p.types, named, types.NewPointer(named))
		}
	}
	for _, file := range pkg.files {
		rel, err := filepath.Rel(p.root, p.fset.Position(file.Pos()).Filename)
		if err != nil {
			return err
		}
		imports := map[string]*closureDeclaration{}
		comments := ast.NewCommentMap(p.fset, file, file.Comments)
		makeDecl := func(node ast.Node, id string) (*closureDeclaration, error) {
			var formatted bytes.Buffer
			if err := format.Node(&formatted, p.fset, &printer.CommentedNode{Node: node, Comments: comments.Filter(node).Comments()}); err != nil {
				return nil, err
			}
			return &closureDeclaration{id: id, file: filepath.ToSlash(rel), source: formatted.String(), node: node, pkg: pkg, imports: imports}, nil
		}
		for _, spec := range file.Imports {
			obj, _ := pkg.info.Implicits[spec].(*types.PkgName)
			if spec.Name != nil {
				obj, _ = pkg.info.Defs[spec.Name].(*types.PkgName)
			}
			if obj == nil {
				return errors.New("closure unresolved import")
			}
			node := &ast.GenDecl{Tok: token.IMPORT, Specs: []ast.Spec{spec}}
			decl, err := makeDecl(node, pkg.meta.ImportPath+"#import:"+filepath.ToSlash(rel)+":"+obj.Imported().Path())
			if err != nil {
				return err
			}
			imports[obj.Imported().Path()] = decl
			// Every import participates in package initialization, including
			// blank imports and imports used only by otherwise unreached code.
			pkg.initializers = append(pkg.initializers, decl)
		}
		for declarationIndex, node := range file.Decls {
			var objects []types.Object
			implicit := false
			switch node := node.(type) {
			case *ast.FuncDecl:
				if node.Name.Name != "init" {
					objects = append(objects, pkg.info.Defs[node.Name])
				} else {
					implicit = true
				}
			case *ast.GenDecl:
				if node.Tok == token.IMPORT {
					continue
				}
				for _, spec := range node.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						objects = append(objects, pkg.info.Defs[spec.Name])
					case *ast.ValueSpec:
						if node.Tok == token.VAR {
							for _, value := range spec.Values {
								ast.Inspect(value, func(child ast.Node) bool {
									if _, literal := child.(*ast.FuncLit); literal {
										return false
									}
									if _, call := child.(*ast.CallExpr); call {
										implicit = true
									}
									return true
								})
							}
						}
						for _, name := range spec.Names {
							if name.Name != "_" {
								objects = append(objects, pkg.info.Defs[name])
							}
						}
					}
				}
			}
			if len(objects) == 0 && !implicit {
				continue
			}
			keys := make([]string, 0, len(objects))
			for _, obj := range objects {
				key := closureObjectKey(obj)
				if key == "" {
					return errors.New("closure unresolved declaration")
				}
				keys = append(keys, key)
			}
			sort.Strings(keys)
			if len(keys) == 0 {
				keys = append(keys, fmt.Sprintf("%s#initializer:%s:%d", pkg.meta.ImportPath, filepath.ToSlash(rel), declarationIndex))
			}
			decl, err := makeDecl(node, strings.Join(keys, ","))
			if err != nil {
				return err
			}
			for _, key := range keys {
				if p.declarations[key] != nil {
					return errors.New("closure duplicate declaration")
				}
				p.declarations[key] = decl
			}
			for _, obj := range objects {
				p.objects[obj] = decl
			}
			if implicit {
				pkg.initializers = append(pkg.initializers, decl)
			}
			// Package scope alone misses anonymous structs and local named types
			// whose embedded methods jointly implement a reached interface.
			// Bind their type syntax without pulling unrelated factory bodies.
			concreteIndex := 0
			var concreteErr error
			ast.Inspect(node, func(child ast.Node) bool {
				if concreteErr != nil {
					return false
				}
				var typ types.Type
				var obj *types.TypeName
				switch child := child.(type) {
				case *ast.StructType:
					typ = pkg.info.Types[child].Type
				case *ast.TypeSpec:
					obj, _ = pkg.info.Defs[child.Name].(*types.TypeName)
					if obj != nil && !obj.IsAlias() && p.objects[obj] == nil {
						typ = obj.Type()
					}
				}
				if typ == nil {
					return true
				}
				if _, iface := typ.Underlying().(*types.Interface); iface {
					return true
				}
				concreteIndex++
				synthetic, err := makeDecl(child, fmt.Sprintf("%s#concrete-type:%d", decl.id, concreteIndex))
				if err != nil {
					concreteErr = err
					return false
				}
				p.typeDeclarations[typ] = synthetic
				if obj != nil {
					p.objects[obj] = synthetic
				}
				p.types = append(p.types, typ, types.NewPointer(typ))
				return true
			})
			if concreteErr != nil {
				return concreteErr
			}
		}
	}
	return nil
}

func (p *closureProgram) closure(roots ...string) (closureResult, error) {
	return p.closureConfigured("", nil, roots...)
}

// GORM's method dispatch lives outside this module. The registry explicitly
// opts into its pinned TableName/lifecycle interfaces for reached model types.
// Other reflective APIs, model methods, and external source remain excluded.
func (p *closureProgram) closureWithGORMModels(modelPackage string, roots ...string) (closureResult, error) {
	if p.packages[modelPackage] == nil {
		return closureResult{}, errors.New("closure model package unavailable")
	}
	contracts := []struct {
		path  string
		names []string
	}{
		{"gorm.io/gorm/schema", []string{"Tabler", "TablerWithNamer"}},
		{"gorm.io/gorm/callbacks", []string{"BeforeCreateInterface", "AfterCreateInterface", "BeforeUpdateInterface", "AfterUpdateInterface", "BeforeSaveInterface", "AfterSaveInterface", "BeforeDeleteInterface", "AfterDeleteInterface", "AfterFindInterface"}},
	}
	var interfaces []*types.Interface
	for _, contract := range contracts {
		pkg, err := p.Import(contract.path)
		if err != nil {
			return closureResult{}, errors.New("closure GORM contract export unavailable")
		}
		for _, name := range contract.names {
			obj, ok := pkg.Scope().Lookup(name).(*types.TypeName)
			if !ok {
				return closureResult{}, errors.New("closure GORM contract missing")
			}
			iface, ok := obj.Type().Underlying().(*types.Interface)
			if !ok || iface.NumMethods() != 1 {
				return closureResult{}, errors.New("closure GORM contract shape")
			}
			interfaces = append(interfaces, iface.Complete())
		}
	}
	return p.closureConfigured(modelPackage, interfaces, roots...)
}

func (p *closureProgram) closureConfigured(modelPackage string, gormInterfaces []*types.Interface, roots ...string) (closureResult, error) {
	if len(roots) == 0 {
		return closureResult{}, errors.New("closure roots required")
	}
	queue := []closureVisit{}
	for _, root := range roots {
		decl := p.declarations[root]
		if decl == nil {
			return closureResult{}, fmt.Errorf("closure root not found: %s", root)
		}
		queue = append(queue, closureVisit{&closureCallable{decl: decl, node: decl.node, key: decl.id}, nil})
	}
	seen := map[*closureDeclaration]bool{}
	visited := map[string]bool{}
	initialized := map[string]bool{}
	for len(queue) != 0 {
		visit := queue[0]
		queue = queue[1:]
		decl := visit.callable.decl
		visitKey := decl.file + ":" + visit.callable.key + "{" + closureBindingsKey(visit.bindings) + "}"
		if visited[visitKey] {
			continue
		}
		if len(visited) >= 50000 {
			return closureResult{}, errors.New("closure visit context limit")
		}
		visited[visitKey] = true
		if !closureBindingsImmutable(visit.callable.node, decl.pkg.info, visit.bindings) {
			return closureResult{}, fmt.Errorf("%s: %s: closure callback reassignment or address escape", decl.file, decl.id)
		}
		seen[decl] = true
		addDecl := func(reached *closureDeclaration) {
			queue = append(queue, closureVisit{&closureCallable{decl: reached, node: reached.node, key: reached.id}, nil})
		}
		var addInitializers func(*closurePackage)
		addInitializers = func(pkg *closurePackage) {
			if initialized[pkg.meta.ImportPath] {
				return
			}
			initialized[pkg.meta.ImportPath] = true
			for _, initializer := range pkg.initializers {
				addDecl(initializer)
			}
			for _, imported := range pkg.typed.Imports() {
				if modulePackage := p.packages[imported.Path()]; modulePackage != nil {
					addInitializers(modulePackage)
				}
			}
		}
		addInitializers(decl.pkg)
		addGORMMethods := func(obj types.Object) {
			typeName, ok := obj.(*types.TypeName)
			if !ok || typeName.Pkg() == nil || typeName.Pkg().Path() != modelPackage {
				return
			}
			for _, candidate := range []types.Type{typeName.Type(), types.NewPointer(typeName.Type())} {
				for _, iface := range gormInterfaces {
					if !types.Implements(candidate, iface) {
						continue
					}
					method := types.NewMethodSet(candidate).Lookup(iface.Method(0).Pkg(), iface.Method(0).Name())
					if method == nil {
						continue
					}
					if target := p.functionCallable(method.Obj().(*types.Func)); target != nil {
						queue = append(queue, closureVisit{target, nil})
					}
				}
			}
		}
		addObject := func(obj types.Object) {
			addGORMMethods(obj)
			if fn, ok := obj.(*types.Func); ok {
				obj = fn.Origin()
			}
			if reached := p.objects[obj]; reached != nil {
				addDecl(reached)
			}
			if obj != nil && obj.Pkg() != nil {
				if imported := decl.imports[obj.Pkg().Path()]; imported != nil {
					addDecl(imported)
				}
			}
			if name, ok := obj.(*types.PkgName); ok {
				if imported := decl.imports[name.Imported().Path()]; imported != nil {
					addDecl(imported)
				}
			}
		}
		addImplementationType := func(typ types.Type) {
			typ = types.Unalias(typ)
			if pointer, ok := typ.(*types.Pointer); ok {
				typ = types.Unalias(pointer.Elem())
			}
			if named, ok := typ.(*types.Named); ok {
				addObject(named.Origin().Obj())
			} else if synthetic := p.typeDeclarations[typ]; synthetic != nil {
				addDecl(synthetic)
			}
		}
		var callErr error
		boundRefs := map[ast.Expr]bool{}
		ast.Inspect(visit.callable.node, func(node ast.Node) bool {
			if callErr != nil {
				return false
			}
			if call, ok := node.(*ast.CallExpr); ok {
				if decl.pkg.info.Types[call.Fun].IsType() {
					return true
				}
				fun := closureUnparen(call.Fun)
				var obj types.Object
				var selection *types.Selection
				switch fun := fun.(type) {
				case *ast.Ident:
					obj = decl.pkg.info.Uses[fun]
				case *ast.SelectorExpr:
					selection = decl.pkg.info.Selections[fun]
					if selection != nil {
						obj = selection.Obj()
					} else {
						obj = decl.pkg.info.Uses[fun.Sel]
					}
				}
				if _, builtin := obj.(*types.Builtin); builtin {
					return true
				}
				targets := []*closureCallable{}
				if fn, resolved := obj.(*types.Func); resolved {
					if fn.Pkg() != nil && fn.Pkg().Path() == "reflect" && (fn.Name() == "Call" || fn.Name() == "CallSlice") {
						callErr = errors.New("closure reflection call is unsupported")
						return false
					}
					if selection != nil {
						if iface, ok := selection.Recv().Underlying().(*types.Interface); ok {
							iface.Complete()
							for _, candidate := range p.types {
								if types.Implements(candidate, iface) {
									addImplementationType(candidate)
									method := types.NewMethodSet(candidate).Lookup(fn.Pkg(), fn.Name())
									if method != nil {
										if target := p.functionCallable(method.Obj().(*types.Func)); target != nil {
											targets = append(targets, target)
										}
									}
								}
							}
						}
					}
					if target := p.functionCallable(fn); target != nil {
						targets = append(targets, target)
					}
				} else {
					target, err := p.valueCallable(decl, fun, visit.bindings)
					if err != nil {
						callErr = err
						return false
					}
					targets = append(targets, target)
				}
				if len(targets) != 0 {
					boundRefs[fun] = true
					args := call.Args
					if selection != nil && selection.Kind() == types.MethodExpr {
						args = args[1:]
					}
					for _, target := range targets {
						bound, err := p.bindClosureCall(decl, target, args, visit.bindings)
						if err != nil {
							callErr = err
							return false
						}
						queue = append(queue, bound)
						for i := 0; i < target.sig.Params().Len() && i < len(args); i++ {
							if _, callback := target.sig.Params().At(i).Type().Underlying().(*types.Signature); callback {
								boundRefs[closureUnparen(args[i])] = true
							}
						}
					}
				}
			}
			if literal, ok := node.(*ast.FuncLit); ok {
				if literal == visit.callable.node {
					return true
				}
				if !boundRefs[literal] {
					callable, err := p.valueCallable(decl, literal, visit.bindings)
					if err != nil {
						callErr = err
						return false
					}
					queue = append(queue, closureVisit{callable, callable.captured})
				}
				return false
			}
			if ident, ok := node.(*ast.Ident); ok {
				addGORMMethods(decl.pkg.info.Defs[ident])
				if !boundRefs[ident] {
					addObject(decl.pkg.info.Uses[ident])
				}
			}
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if boundRefs[selector] {
				// Keep the import/type receiver uses, but do not enqueue this
				// resolved callee again with an empty callback context.
				boundRefs[selector.Sel] = true
				return true
			}
			selection := decl.pkg.info.Selections[selector]
			if selection == nil || selection.Kind() == types.FieldVal {
				return true
			}
			addObject(selection.Obj())
			iface, ok := selection.Recv().Underlying().(*types.Interface)
			if !ok {
				return true
			}
			iface.Complete()
			// Only this resolved interface method is dispatched. A same-spelled
			// method on a non-implementer or unrelated String method is excluded.
			for _, candidate := range p.types {
				if types.Implements(candidate, iface) {
					addImplementationType(candidate)
					method := types.NewMethodSet(candidate).Lookup(selection.Obj().Pkg(), selection.Obj().Name())
					if method != nil {
						addObject(method.Obj())
					}
				}
			}
			return true
		})
		if callErr != nil {
			return closureResult{}, fmt.Errorf("%s: %s: %w", decl.file, decl.id, callErr)
		}
	}
	type hashedDeclaration struct{ File, ID, Source string }
	hashed := make([]hashedDeclaration, 0, len(seen))
	fileSet := map[string]bool{}
	for decl := range seen {
		hashed = append(hashed, hashedDeclaration{decl.file, decl.id, decl.source})
		fileSet[decl.file] = true
	}
	sort.Slice(hashed, func(i, j int) bool {
		if hashed[i].File != hashed[j].File {
			return hashed[i].File < hashed[j].File
		}
		return hashed[i].ID < hashed[j].ID
	})
	raw, err := json.Marshal(hashed)
	if err != nil {
		return closureResult{}, err
	}
	digest := sha256.Sum256(raw)
	result := closureResult{SHA256: hex.EncodeToString(digest[:])}
	for _, decl := range hashed {
		result.Declarations = append(result.Declarations, decl.File+":"+decl.ID)
	}
	for file := range fileSet {
		result.Files = append(result.Files, file)
	}
	sort.Strings(result.Files)
	return result, nil
}

func requireClosureCap(result closureResult, maximum int) error {
	if maximum < 1 || len(result.Files) > maximum {
		return fmt.Errorf("closure file cap %d exceeded by %d files:\n%s", maximum, len(result.Files), strings.Join(result.Files, "\n"))
	}
	return nil
}

func copyClosureFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	err := filepath.WalkDir("testdata/closure", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("testdata/closure", path)
		if err != nil {
			return err
		}
		destination := filepath.Join(root, rel)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestClosureResolvedDependencies(t *testing.T) {
	for _, tc := range []struct {
		name, file, before, after string
		changesHash               bool
	}{
		{"cross-package-callee", "dep/callee.go", "return Settings + Offset", "return Settings + Offset + 1", true},
		{"package-variable", "dep/data.go", "var Settings = 3", "var Settings = 7", true},
		{"package-constant", "dep/data.go", "const Offset = 2", "const Offset = 4", true},
		{"named-type", "dep/data.go", "type Number int", "type Number int64", true},
		{"receiver-method", "dep/method.go", "return Number(Read())", "return Number(Read() + 1)", true},
		{"interface-value-implementation", "impl/one.go", "return 11", "return 12", true},
		{"interface-pointer-implementation", "impl/two.go", "return 21", "return 22", true},
		{"unrelated-string", "unrelated/string.go", "return \"unrelated\"", "return \"changed\"", false},
		{"same-name-non-implementation", "unrelated/string.go", "return \"wrong signature\"", "return \"changed signature body\"", false},
		{"generic-method", "dep/generic.go", "return v.Value", "return v.Value + 1", true},
		{"pointer-method", "dep/pointer.go", "return 31", "return 32", true},
		{"deferred-callee", "dep/deferred.go", "return 41", "return 42", true},
		{"shadowed-local", "entry/unrelated.go", "var local = 99", "var local = 100", false},
		{"first-callback-context", "callbacks/first.go", "return 51", "return 52", true},
		{"second-callback-context", "callbacks/second.go", "return 61", "return 62", true},
		{"literal-callback", "callbacks/third.go", "return 71", "return 72", true},
		{"promoted-implementation-type", "impl/embedded.go", "struct{ One }", "struct{ *Two }", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := copyClosureFixture(t)
			program, err := loadClosureProgram(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			before, err := program.closure("closurefixture.test/entry.Root")
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(root, tc.file)
			data, err := os.ReadFile(file)
			if err != nil || strings.Count(string(data), tc.before) != 1 {
				t.Fatalf("fixture source: %v", err)
			}
			if err := os.WriteFile(file, []byte(strings.Replace(string(data), tc.before, tc.after, 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			program, err = loadClosureProgram(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			after, err := program.closure("closurefixture.test/entry.Root")
			if err != nil || (before.SHA256 != after.SHA256) != tc.changesHash {
				t.Fatalf("unexpected closure change: %v; before=%s after=%s", err, before.SHA256, after.SHA256)
			}
			if strings.Contains(strings.Join(after.Files, "\n"), "unrelated/") {
				t.Fatal("unrelated method entered closure")
			}
		})
	}
}

func TestClosureRejectsUnresolvedDynamicCalls(t *testing.T) {
	program, err := loadClosureProgram(context.Background(), copyClosureFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"Parameter", "Field", "Variable", "Reflection"} {
		if _, err := program.closure("closurefixture.test/dynamic." + root); err == nil {
			t.Fatalf("accepted unresolved dynamic call %s", root)
		}
	}
	if _, err := program.closure("closurefixture.test/callbacks.Unknown"); err == nil {
		t.Fatal("known callback context masked another unresolved context")
	}
	for _, root := range []string{"BadReassigned", "BadEscaped", "BadNested", "BadRange", "BadLocal", "BadLocalEscape"} {
		if _, err := program.closure("closurefixture.test/callbacks." + root); err == nil {
			t.Fatalf("accepted mutable callback %s", root)
		}
	}
}

func TestClosureImportIdentityAndInitialization(t *testing.T) {
	root := copyClosureFixture(t)
	program, err := loadClosureProgram(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := program.closure("closurefixture.test/entry.ImportA", "closurefixture.test/entry.ImportB")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		second, err := program.closure("closurefixture.test/entry.ImportB", "closurefixture.test/entry.ImportA")
		if err != nil || first.SHA256 != second.SHA256 {
			t.Fatalf("order-dependent imports: %v", err)
		}
	}
	for _, file := range []string{"entry/import_a.go", "entry/import_b.go"} {
		if !strings.Contains(strings.Join(first.Declarations, "\n"), "#import:"+file+":closurefixture.test/dep") {
			t.Fatalf("lost import declaration %s", file)
		}
	}
	for _, tc := range []struct{ file, old, new string }{
		{"initialization/init.go", "Seed = \"B\"", "Seed = \"D\""},
		{"initialization/blank.go", "Seed = \"C\"", "Seed = \"E\""},
		{"entry/import_b.go", "second \"closurefixture.test/dep\"", "second /* binding */ \"closurefixture.test/dep\""},
	} {
		t.Run(tc.file, func(t *testing.T) {
			fixture := copyClosureFixture(t)
			p, err := loadClosureProgram(context.Background(), fixture)
			if err != nil {
				t.Fatal(err)
			}
			roots := []string{"closurefixture.test/initialization.Root", "closurefixture.test/entry.ImportA", "closurefixture.test/entry.ImportB"}
			before, err := p.closure(roots...)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(fixture, tc.file)
			data, err := os.ReadFile(path)
			if err != nil || strings.Count(string(data), tc.old) != 1 {
				t.Fatal("invalid fixture replacement")
			}
			if err := os.WriteFile(path, []byte(strings.Replace(string(data), tc.old, tc.new, 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			p, err = loadClosureProgram(context.Background(), fixture)
			if err != nil {
				t.Fatal(err)
			}
			after, err := p.closure(roots...)
			if err != nil || before.SHA256 == after.SHA256 {
				t.Fatalf("implicit dependency missing: %v", err)
			}
		})
	}
}

func TestClosureAnonymousAndLocalImplementations(t *testing.T) {
	for _, tc := range []struct{ name, file, old, new string }{
		{"anonymous-method", "anonymous/methods.go", "return 81", "return 91"},
		{"local-method", "anonymous/methods.go", "return 83", "return 93"},
		{"anonymous-shape", "anonymous/root.go", "\t\tB\n", "\t\tC\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := copyClosureFixture(t)
			p, err := loadClosureProgram(context.Background(), fixture)
			if err != nil {
				t.Fatal(err)
			}
			before, err := p.closure("closurefixture.test/anonymous.Root")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(fixture, tc.file)
			data, err := os.ReadFile(path)
			data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
			if err != nil || strings.Count(string(data), tc.old) != 1 {
				t.Fatal("invalid fixture replacement")
			}
			if err := os.WriteFile(path, []byte(strings.Replace(string(data), tc.old, tc.new, 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			p, err = loadClosureProgram(context.Background(), fixture)
			if err != nil {
				t.Fatal(err)
			}
			after, err := p.closure("closurefixture.test/anonymous.Root")
			if err != nil || before.SHA256 == after.SHA256 {
				t.Fatalf("implementation dependency missing: %v", err)
			}
			if strings.Contains(strings.Join(after.Declarations, "\n")+"\n", ":closurefixture.test/anonymous.Factory\n") {
				t.Fatal("unrelated factory body included")
			}
		})
	}
}

func TestClosureExplicitGORMModelContract(t *testing.T) {
	for _, tc := range []struct {
		name, old, new string
		changes        bool
	}{
		{"table-name", `return "fixture_rows"`, `return "changed_rows"`, true},
		{"table-name-with-namer", `return "named_rows"`, `return "changed_named_rows"`, true},
		{"lifecycle-hook", "BeforeCreate(*gorm.DB) error { return nil }", "BeforeCreate(*gorm.DB) error { _ = 1; return nil }", true},
		{"promoted-hook", "AfterFind(*gorm.DB) error { return nil }", "AfterFind(*gorm.DB) error { _ = 2; return nil }", true},
		{"unrelated-string", `return "excluded string"`, `return "changed string"`, false},
		{"unreached-model", `return "unreached_rows"`, `return "changed_unreached"`, false},
		{"wrong-hook-signature", "BeforeCreate(string) error { return nil }", "BeforeCreate(string) error { _ = 3; return nil }", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := copyClosureFixture(t)
			p, err := loadClosureProgram(context.Background(), fixture)
			if err != nil {
				t.Fatal(err)
			}
			before, err := p.closureWithGORMModels("closurefixture.test/ormmodels", "closurefixture.test/ormroot.Root")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(fixture, "ormmodels/model.go")
			data, err := os.ReadFile(path)
			if err != nil || strings.Count(string(data), tc.old) != 1 {
				t.Fatal("invalid fixture replacement")
			}
			if err := os.WriteFile(path, []byte(strings.Replace(string(data), tc.old, tc.new, 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			p, err = loadClosureProgram(context.Background(), fixture)
			if err != nil {
				t.Fatal(err)
			}
			after, err := p.closureWithGORMModels("closurefixture.test/ormmodels", "closurefixture.test/ormroot.Root")
			if err != nil || (before.SHA256 != after.SHA256) != tc.changes {
				t.Fatalf("model contract change=%v: %v", tc.changes, err)
			}
		})
	}
}

func TestClosureDeterministicRootsAndCap(t *testing.T) {
	program, err := loadClosureProgram(context.Background(), copyClosureFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	first, err := program.closure("closurefixture.test/entry.Root", "closurefixture.test/dep.Read")
	if err != nil {
		t.Fatal(err)
	}
	second, err := program.closure("closurefixture.test/dep.Read", "closurefixture.test/entry.Root", "closurefixture.test/entry.Root")
	if err != nil || first.SHA256 != second.SHA256 || strings.Join(first.Files, "\n") != strings.Join(second.Files, "\n") {
		t.Fatalf("closure is not deterministic: %v", err)
	}
	for _, required := range []string{"dep/callee.go", "dep/data.go", "dep/method.go", "entry/root.go", "impl/one.go", "impl/two.go", "runner/interface.go"} {
		if !strings.Contains("\n"+strings.Join(first.Files, "\n")+"\n", "\n"+required+"\n") {
			t.Fatalf("missing resolved closure file %s", required)
		}
	}
	if err := requireClosureCap(first, len(first.Files)); err != nil {
		t.Fatal(err)
	}
	if err := requireClosureCap(first, len(first.Files)-1); err == nil || !strings.Contains(err.Error(), strings.Join(first.Files, "\n")) {
		t.Fatal("cap did not refuse with complete file inventory")
	}
	if _, err := program.closure("closurefixture.test/entry.Missing"); err == nil {
		t.Fatal("unresolved root accepted")
	}
	if _, err := program.closure(); err == nil {
		t.Fatal("empty roots accepted")
	}
}
