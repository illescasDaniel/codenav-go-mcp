package gosrc

import (
	"go/ast"
	"go/parser"
	"go/token"
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
