package gosrc

import (
	"go/ast"
	"go/token"
)

// qualifierIdent is the identifier that ends the qualifier of x.M: x itself, the T of (*T), T[...] or pkg.T, or
// the field T of u.T. nil for an expression that is not a name (a call, a literal).
func qualifierIdent(e ast.Expr) *ast.Ident {
	switch t := e.(type) {
	case *ast.Ident:
		return t
	case *ast.SelectorExpr:
		return t.Sel
	case *ast.ParenExpr:
		return qualifierIdent(t.X)
	case *ast.StarExpr:
		return qualifierIdent(t.X)
	case *ast.IndexExpr:
		return qualifierIdent(t.X)
	case *ast.IndexListExpr:
		return qualifierIdent(t.X)
	}
	return nil
}

// TypeNameAt reports whether the name starting at off is the name in a type declaration.
func (f *File) TypeNameAt(off int) bool {
	found := false
	ast.Inspect(f.ast, func(n ast.Node) bool {
		if ts, isType := n.(*ast.TypeSpec); isType && f.off(ts.Name.Pos()) == off {
			found = true
		}
		return !found
	})
	return found
}

// BodyDeclLine returns the 1-based line of a declaration of name in the outermost block of the body of
// the function whose name starts at off, or 0 when there is none. Such a declaration shares the scope of
// the parameters, so a parameter with the same name cannot be added: a var, const or type declaration, or
// a := whose only new variable would be name (`b, err := f()` assigns to the parameter b and compiles).
func (f *File) BodyDeclLine(off int, name string) int {
	for _, decl := range f.ast.Decls {
		d, isFunc := decl.(*ast.FuncDecl)
		if !isFunc || d.Body == nil || f.off(d.Name.Pos()) != off {
			continue
		}
		outer := map[string]bool{}
		for _, fl := range []*ast.FieldList{d.Type.Params, d.Type.Results} {
			if fl == nil {
				continue
			}
			for _, field := range fl.List {
				for _, n := range field.Names {
					outer[n.Name] = true
				}
			}
		}
		line := func(p token.Pos) int { return f.fset.Position(p).Line }
		for _, st := range d.Body.List {
			switch s := st.(type) {
			case *ast.DeclStmt:
				gd, isGen := s.Decl.(*ast.GenDecl)
				if !isGen {
					continue
				}
				for _, spec := range gd.Specs {
					switch sp := spec.(type) {
					case *ast.ValueSpec:
						for _, n := range sp.Names {
							if n.Name == name {
								return line(n.Pos())
							}
						}
					case *ast.TypeSpec:
						if sp.Name.Name == name {
							return line(sp.Name.Pos())
						}
					}
				}
			case *ast.AssignStmt:
				if s.Tok != token.DEFINE {
					continue
				}
				// With name a parameter, the := has no new variable when every other name is a parameter.
				hit, otherNew := token.NoPos, 0
				for _, lhs := range s.Lhs {
					id, isIdent := lhs.(*ast.Ident)
					if !isIdent || id.Name == "_" {
						continue
					}
					if id.Name == name {
						hit = id.Pos()
					} else if !outer[id.Name] {
						otherNew++
					}
				}
				if hit.IsValid() && otherNew == 0 {
					return line(hit)
				}
			}
		}
		return 0
	}
	return 0
}

// BodyUses returns the byte range of the body of the function whose name starts at off, and the offsets of the
// identifiers named name in that body. A selector's field or method name (x.name) is not a use of a variable
// and is left out. ok is false when no function with a body starts there.
func (f *File) BodyUses(off int, name string) (start, end int, uses []int, ok bool) {
	for _, decl := range f.ast.Decls {
		d, isFunc := decl.(*ast.FuncDecl)
		if !isFunc || d.Body == nil || f.off(d.Name.Pos()) != off {
			continue
		}
		selectorNames := map[*ast.Ident]bool{}
		// ast.Inspect visits a selector before its parts, so its name is marked before it is reached.
		ast.Inspect(d.Body, func(n ast.Node) bool {
			switch t := n.(type) {
			case *ast.SelectorExpr:
				selectorNames[t.Sel] = true
			case *ast.Ident:
				if t.Name == name && !selectorNames[t] {
					uses = append(uses, f.off(t.Pos()))
				}
			}
			return true
		})
		return f.off(d.Body.Lbrace), f.off(d.Body.Rbrace), uses, true
	}
	return 0, 0, nil, false
}
