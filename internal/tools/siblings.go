package tools

import (
	"context"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
)

// declSignatures maps each top-level declaration of a Go file to the source text callers depend on: a
// function or method without its body, a type, var or const spec as written. The key tells
// declarations apart ("func (T) M", "type T", "var x"). ok is false when the file does not parse.
func declSignatures(path, src string) (sigs map[string]string, names map[string][]string, ok bool) {
	sigs, names, _, ok = declObjects(path, src)
	return
}

// declObjects is declSignatures that also lists, per declaration, the objects it declares as typed keys:
// "o:Name" (package-level func, type, var, const), "m:Recv.Name" (method, interface method) and "f:Name"
// (struct field), the form objectKey gives the objects the type checker resolves a use to.
func declObjects(path, src string) (sigs map[string]string, names map[string][]string, objs map[string][]string, ok bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if f == nil || err != nil {
		return nil, nil, nil, false
	}
	text := func(a, b token.Pos) string { return src[fset.Position(a).Offset:fset.Position(b).Offset] }
	sigs, names, objs = map[string]string{}, map[string][]string{}, map[string][]string{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			end := d.End()
			if d.Body != nil {
				end = d.Body.Lbrace
			}
			key := "func " + d.Name.Name
			objs[key] = []string{"o:" + d.Name.Name}
			if d.Recv != nil && len(d.Recv.List) == 1 {
				key = "func (" + strings.TrimSpace(text(d.Recv.List[0].Type.Pos(), d.Recv.List[0].Type.End())) + ") " + d.Name.Name
				objs[key] = []string{"m:" + recvTypeName(d.Recv.List[0].Type) + "." + d.Name.Name}
			}
			sigs[key] = text(d.Type.Pos(), end)
			names[key] = []string{d.Name.Name}
		case *ast.GenDecl:
			for _, sp := range d.Specs {
				switch sp := sp.(type) {
				case *ast.TypeSpec:
					key := "type " + sp.Name.Name
					sigs[key] = text(sp.Pos(), sp.End())
					ns, ob := []string{sp.Name.Name}, []string{"o:" + sp.Name.Name}
					iface := false
					if _, ok := sp.Type.(*ast.InterfaceType); ok {
						iface = true
					}
					ast.Inspect(sp.Type, func(n ast.Node) bool {
						if fl, ok := n.(*ast.Field); ok {
							for _, id := range fl.Names {
								ns = append(ns, id.Name)
								if iface {
									ob = append(ob, "m:"+sp.Name.Name+"."+id.Name)
								} else {
									ob = append(ob, "f:"+id.Name)
								}
							}
						}
						return true
					})
					names[key] = ns
					objs[key] = ob
				case *ast.ValueSpec:
					for _, id := range sp.Names {
						key := d.Tok.String() + " " + id.Name
						sigs[key] = text(sp.Pos(), sp.End())
						names[key] = []string{id.Name}
						objs[key] = []string{"o:" + id.Name}
					}
				}
			}
		}
	}
	return sigs, names, objs, true
}

// recvTypeName is the type name of a method receiver expression (`*T`, `T[K]`, `(T)`).
func recvTypeName(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.Ident:
			return x.Name
		default:
			return ""
		}
	}
}

// changedDeclNames returns the names of the declarations the plan adds, removes or changes (a function's
// body does not count: callers do not depend on it) in the files below dir. ok is false when some file
// cannot be compared, which means "unknown".
func changedDeclNames(plan *edits.EditPlan, dir string) (names map[string]bool, ok bool) {
	names, _, ok = changedDecls(plan, dir)
	return names, ok
}

// changedDecls is changedDeclNames that also returns, per package directory, the typed keys (see
// declObjects) of the objects the plan removes or changes. Objects it only adds are left out: nothing can
// refer to them yet.
func changedDecls(plan *edits.EditPlan, dir string) (names map[string]bool, objs map[string]map[string]bool, ok bool) {
	names, objs = map[string]bool{}, map[string]map[string]bool{}
	for _, c := range plan.Changes {
		if filepath.Ext(c.Path) != ".go" {
			continue
		}
		if rel, err := filepath.Rel(dir, c.Path); err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		oldS, oldN, oldO, ok1 := map[string]string{}, map[string][]string{}, map[string][]string{}, true
		newS, newN, _, ok2 := map[string]string{}, map[string][]string{}, map[string][]string{}, true
		if c.Old != nil {
			oldS, oldN, oldO, ok1 = declObjects(c.Path, *c.Old)
		}
		if c.New != nil {
			newS, newN, _, ok2 = declObjects(c.Path, *c.New)
		}
		if !ok1 || !ok2 {
			return nil, nil, false
		}
		pd := filepath.Dir(c.Path)
		if objs[pd] == nil {
			objs[pd] = map[string]bool{}
		}
		for k, sig := range oldS {
			if newSig, still := newS[k]; !still || newSig != sig {
				for _, n := range oldN[k] {
					names[n] = true
				}
				for _, o := range oldO[k] {
					objs[pd][o] = true
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
	return names, objs, true
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

// objectKey is the typed key (see declObjects) of an object the type checker resolved a use to, "" for
// objects that no key describes (locals, parameters, imports).
func objectKey(obj types.Object) string {
	if obj == nil || obj.Pkg() == nil {
		return ""
	}
	switch o := obj.(type) {
	case *types.Func:
		o = o.Origin()
		if sig, ok := o.Type().(*types.Signature); ok && sig.Recv() != nil {
			t := sig.Recv().Type()
			if p, ok := t.(*types.Pointer); ok {
				t = p.Elem()
			}
			if n, ok := t.(*types.Named); ok {
				return "m:" + n.Obj().Name() + "." + o.Name()
			}
			return ""
		}
	case *types.Var:
		o = o.Origin()
		if o.IsField() {
			return "f:" + o.Name()
		}
	}
	if obj.Parent() == obj.Pkg().Scope() {
		return "o:" + obj.Name()
	}
	return ""
}

// usedObjects type-checks the Go package(s) in dir (their tests too) and returns, per file, the set of
// "<import path>|<key>" of the objects of other packages that the file uses. nil when the directory cannot
// be loaded, which means "unknown".
func usedObjects(ctx context.Context, dir string) map[string]map[string]bool {
	cfg := &packages.Config{
		Context: ctx,
		Dir:     dir,
		Tests:   true,
		Mode:    packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedImports | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Env:     append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=readonly"),
	}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil || len(pkgs) == 0 {
		return nil
	}
	out := map[string]map[string]bool{}
	for _, p := range pkgs {
		if p.TypesInfo == nil || p.Fset == nil {
			return nil
		}
		for _, f := range p.Syntax {
			name := p.Fset.Position(f.Pos()).Filename
			set := out[name]
			if set == nil {
				set = map[string]bool{}
				out[name] = set
			}
			ast.Inspect(f, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				if obj := p.TypesInfo.Uses[id]; obj != nil && obj.Pkg() != nil && obj.Pkg() != p.Types {
					if k := objectKey(obj); k != "" {
						set[obj.Pkg().Path()+"|"+k] = true
					}
				}
				return true
			})
		}
	}
	return out
}
