package gosrc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path"
	"strings"
)

// NonMemberIdents returns the positions (line<<20 | byte column, both 1-based) of the identifiers
// called name that cannot be a struct field, method or interface method: a bare identifier (a call
// `Area(x)`, a package-level `func Area`, a local variable) is looked up in a scope, never in a type.
// Member positions are selectors (`x.Area`), method declarations, field and interface-method names,
// composite-literal keys and embedded types. nil when the file does not parse.
func NonMemberIdents(path, text, name string) map[int]bool {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, text, parser.SkipObjectResolution)
	if f == nil || err != nil {
		return nil
	}
	member := map[*ast.Ident]bool{}
	fields := func(l *ast.FieldList) {
		if l == nil {
			return
		}
		for _, fl := range l.List {
			for _, n := range fl.Names {
				member[n] = true
			}
			if len(fl.Names) == 0 {
				ast.Inspect(fl.Type, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok {
						member[id] = true
					}
					return true
				})
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			member[x.Sel] = true
		case *ast.FuncDecl:
			if x.Recv != nil {
				member[x.Name] = true
			}
		case *ast.StructType:
			fields(x.Fields)
		case *ast.InterfaceType:
			fields(x.Methods)
		case *ast.KeyValueExpr:
			if id, ok := x.Key.(*ast.Ident); ok {
				member[id] = true
			}
		}
		return true
	})
	out := map[int]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name && !member[id] {
			p := fset.Position(id.Pos())
			out[p.Line<<20|p.Column] = true
		}
		return true
	})
	return out
}

// NonSelectorIdents returns the positions (as NonMemberIdents) of the identifiers called name that are
// not the selected part of a selector (`pkg.Name`, `x.Name`). From a file of another package, only a
// qualified `pkg.Name` can refer to a package-level symbol, so every other occurrence of the name
// there (a declaration, a local, a call of the package's own function) is an unrelated object.
// nil when the file does not parse.
func NonSelectorIdents(path, text, name string) map[int]bool {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, text, parser.SkipObjectResolution)
	if f == nil || err != nil {
		return nil
	}
	sel := map[*ast.Ident]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if x, ok := n.(*ast.SelectorExpr); ok {
			sel[x.Sel] = true
		}
		return true
	})
	out := map[int]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name && !sel[id] {
			p := fset.Position(id.Pos())
			out[p.Line<<20|p.Column] = true
		}
		return true
	})
	return out
}

// stubImporter gives every import an empty package, so the files of one directory type-check against
// each other (declarations of the package itself resolve) without loading dependencies.
type stubImporter struct{}

func (stubImporter) Import(p string) (*types.Package, error) {
	pkg := types.NewPackage(p, path.Base(p))
	pkg.MarkComplete()
	return pkg, nil
}

// MemberIdentsInDir returns, per file, the positions (line<<20 | byte column, 1-based) of the identifiers
// called name that the type checker resolves to a struct field or a method: another object than a
// package-level symbol of that name (a field declaration, a selector `t.foo`, a keyed literal `T{foo: 1}`,
// a method declaration). files maps the path of each .go file of ONE directory to its text. Imports are
// stubbed, so anything that needs a dependency's types stays unresolved (and is not returned).
func MemberIdentsInDir(files map[string]string, name string) map[string]map[int]bool {
	fset := token.NewFileSet()
	byPkg := map[string][]*ast.File{}
	for p, t := range files {
		if !strings.Contains(t, name) {
			continue
		}
		if f, _ := parser.ParseFile(fset, p, t, 0); f != nil && f.Name != nil {
			byPkg[f.Name.Name] = append(byPkg[f.Name.Name], f)
		}
	}
	// The files that do not mention name are needed for resolving types declared in them.
	for p, t := range files {
		if strings.Contains(t, name) {
			continue
		}
		if f, _ := parser.ParseFile(fset, p, t, 0); f != nil && f.Name != nil {
			if _, ok := byPkg[f.Name.Name]; ok {
				byPkg[f.Name.Name] = append(byPkg[f.Name.Name], f)
			}
		}
	}
	out := map[string]map[int]bool{}
	for pkgName, fs := range byPkg {
		info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
		conf := types.Config{Importer: stubImporter{}, FakeImportC: true, Error: func(error) {}}
		_, _ = conf.Check(pkgName, fset, fs, info)
		mark := func(id *ast.Ident, obj types.Object) {
			if id.Name != name || obj == nil {
				return
			}
			member := false
			switch o := obj.(type) {
			case *types.Var:
				member = o.IsField()
			case *types.Func:
				if sig, ok := o.Type().(*types.Signature); ok && sig.Recv() != nil {
					member = true
				}
			}
			if member {
				p := fset.Position(id.Pos())
				if out[p.Filename] == nil {
					out[p.Filename] = map[int]bool{}
				}
				out[p.Filename][p.Line<<20|p.Column] = true
			}
		}
		for id, obj := range info.Uses {
			mark(id, obj)
		}
		for id, obj := range info.Defs {
			mark(id, obj)
		}
	}
	return out
}
