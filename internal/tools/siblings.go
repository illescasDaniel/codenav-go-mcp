package tools

import (
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
)

// declSignatures maps each top-level declaration of a Go file to the source text callers depend on: a
// function or method without its body, a type, var or const spec as written. The key tells
// declarations apart ("func (T) M", "type T", "var x"). ok is false when the file does not parse.
func declSignatures(path, src string) (sigs map[string]string, names map[string][]string, ok bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if f == nil || err != nil {
		return nil, nil, false
	}
	text := func(a, b token.Pos) string { return src[fset.Position(a).Offset:fset.Position(b).Offset] }
	sigs, names = map[string]string{}, map[string][]string{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			end := d.End()
			if d.Body != nil {
				end = d.Body.Lbrace
			}
			key := "func " + d.Name.Name
			if d.Recv != nil && len(d.Recv.List) == 1 {
				key = "func (" + strings.TrimSpace(text(d.Recv.List[0].Type.Pos(), d.Recv.List[0].Type.End())) + ") " + d.Name.Name
			}
			sigs[key] = text(d.Type.Pos(), end)
			names[key] = []string{d.Name.Name}
		case *ast.GenDecl:
			for _, sp := range d.Specs {
				switch sp := sp.(type) {
				case *ast.TypeSpec:
					key := "type " + sp.Name.Name
					sigs[key] = text(sp.Pos(), sp.End())
					ns := []string{sp.Name.Name}
					ast.Inspect(sp.Type, func(n ast.Node) bool {
						if fl, ok := n.(*ast.Field); ok {
							for _, id := range fl.Names {
								ns = append(ns, id.Name)
							}
						}
						return true
					})
					names[key] = ns
				case *ast.ValueSpec:
					for _, id := range sp.Names {
						key := d.Tok.String() + " " + id.Name
						sigs[key] = text(sp.Pos(), sp.End())
						names[key] = []string{id.Name}
					}
				}
			}
		}
	}
	return sigs, names, true
}

// changedDeclNames returns the names of the declarations the plan adds, removes or changes (a function's
// body does not count: callers do not depend on it) in the files below dir. ok is false when some file
// cannot be compared, which means "unknown".
func changedDeclNames(plan *edits.EditPlan, dir string) (names map[string]bool, ok bool) {
	names = map[string]bool{}
	for _, c := range plan.Changes {
		if filepath.Ext(c.Path) != ".go" {
			continue
		}
		if rel, err := filepath.Rel(dir, c.Path); err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		var oldS, newS map[string]string
		var oldN, newN map[string][]string
		var ok1, ok2 bool
		oldS, oldN, ok1 = map[string]string{}, map[string][]string{}, true
		newS, newN, ok2 = map[string]string{}, map[string][]string{}, true
		if c.Old != nil {
			oldS, oldN, ok1 = declSignatures(c.Path, *c.Old)
		}
		if c.New != nil {
			newS, newN, ok2 = declSignatures(c.Path, *c.New)
		}
		if !ok1 || !ok2 {
			return nil, false
		}
		for k, sig := range oldS {
			if newSig, still := newS[k]; !still || newSig != sig {
				for _, n := range oldN[k] {
					names[n] = true
				}
			}
		}
		for k, sig := range newS {
			if oldSig, was := oldS[k]; !was || oldSig != sig {
				for _, n := range newN[k] {
					names[n] = true
				}
			}
		}
	}
	return names, true
}

// mentionsAny: the Go file uses an identifier from names (a token check, so comments and strings do not count).
func mentionsAny(path string, names map[string]bool) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return true // cannot tell: assume it does
	}
	var s scanner.Scanner
	fset := token.NewFileSet()
	s.Init(fset.AddFile(path, fset.Base(), len(raw)), raw, nil, 0)
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return false
		}
		if tok == token.IDENT && names[lit] {
			return true
		}
	}
}
