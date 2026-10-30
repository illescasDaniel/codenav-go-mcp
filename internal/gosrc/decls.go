package gosrc

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"

	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

// Decl is a top-level declaration (or one spec of a parenthesised const/var/type block)
// located in a source text. Start/End are byte offsets and include the doc comment.
type Decl struct {
	Name       string
	Keyword    string // func, type, const or var
	Method     bool
	Start, End int
	Grouped    bool // one spec inside `const ( ... )`
	MultiName  bool // `var a, b = ...`
	HasDoc     bool
	Closing    int // offset of the closing brace of a struct or interface type, else -1
	BodyKind   string
}

func parse(filename, src string) (*token.FileSet, *ast.File, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, nil, toolerr.Inputf("not valid Go: %v", err)
	}
	return fset, f, nil
}

// FindDeclAt returns the top-level declaration whose name identifier starts at byte offset off.
func FindDeclAt(filename, src string, off int) (*Decl, error) {
	fset, f, err := parse(filename, src)
	if err != nil {
		return nil, err
	}
	at := func(p token.Pos) int { return fset.Position(p).Offset }
	start := func(doc *ast.CommentGroup, p token.Pos) (int, bool) {
		if doc != nil {
			return at(doc.Pos()), true
		}
		return at(p), false
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if at(d.Name.Pos()) != off {
				continue
			}
			s, doc := start(d.Doc, d.Pos())
			return &Decl{Name: d.Name.Name, Keyword: "func", Method: d.Recv != nil, Start: s, End: at(d.End()), HasDoc: doc, Closing: -1}, nil
		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				continue
			}
			for _, spec := range d.Specs {
				var idents []*ast.Ident
				var doc, trailing *ast.CommentGroup
				switch s := spec.(type) {
				case *ast.TypeSpec:
					idents, doc, trailing = []*ast.Ident{s.Name}, s.Doc, s.Comment
				case *ast.ValueSpec:
					idents, doc, trailing = s.Names, s.Doc, s.Comment
				}
				for _, id := range idents {
					if at(id.Pos()) != off {
						continue
					}
					out := &Decl{Name: id.Name, Keyword: d.Tok.String(), MultiName: len(idents) > 1, Closing: -1}
					if ts, ok := spec.(*ast.TypeSpec); ok {
						switch t := ts.Type.(type) {
						case *ast.StructType:
							out.Closing, out.BodyKind = at(t.Fields.Closing), "struct"
						case *ast.InterfaceType:
							out.Closing, out.BodyKind = at(t.Methods.Closing), "interface"
						}
					}
					if !d.Lparen.IsValid() || len(d.Specs) == 1 {
						out.Start, out.HasDoc = start(d.Doc, d.Pos())
						if d.Doc == nil && doc != nil {
							out.Start, out.HasDoc = at(doc.Pos()), true
						}
						out.End = at(d.End())
						return out, nil
					}
					out.Grouped = true
					out.Start, out.HasDoc = start(doc, spec.Pos())
					out.End = at(spec.End())
					if trailing != nil {
						out.End = at(trailing.End())
					}
					return out, nil
				}
			}
		}
	}
	return nil, toolerr.Inputf("that symbol is not a top-level func, method, type, const or var (fields and interface methods are changed with edit)")
}

// DeclNames lists the names a source snippet declares at top level; methods are "Recv.Name".
func DeclNames(src string) ([]string, error) {
	_, f, err := parse("snippet.go", "package p\n"+src)
	if err != nil {
		return nil, err
	}
	return declNames(f), nil
}

func declNames(f *ast.File) []string {
	var out []string
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) == 1 {
				out = append(out, recvName(d.Recv.List[0].Type)+"."+d.Name.Name)
			} else {
				out = append(out, d.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					out = append(out, s.Name.Name)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						out = append(out, n.Name)
					}
				}
			}
		}
	}
	return out
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvName(t.X)
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// FileDeclNames lists the top-level names declared by a whole file.
func FileDeclNames(filename, src string) []string {
	_, f, err := parse(filename, src)
	if err != nil {
		return nil
	}
	return declNames(f)
}

// PackageName reads the package clause of src ("" when unparsable).
func PackageName(src string) string {
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.PackageClauseOnly)
	if err != nil {
		return ""
	}
	return f.Name.Name
}

// ParseImportSpec reads `"os"`, `os`, `import "os"` or `str "strings"` into (alias, path).
func ParseImportSpec(s string) (alias, path string, err error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "import"))
	fields := strings.Fields(s)
	switch len(fields) {
	case 1:
		path = fields[0]
	case 2:
		alias, path = fields[0], fields[1]
	default:
		return "", "", toolerr.Inputf("cannot read import %q; use `encoding/json` or `js \"encoding/json\"`", s)
	}
	if u, uerr := strconv.Unquote(path); uerr == nil {
		path = u
	}
	if path == "" {
		return "", "", toolerr.Inputf("empty import path in %q", s)
	}
	return alias, path, nil
}

// EditImports adds the given imports and, when prune is set, drops the imports that were
// used in oldSrc but are no longer used in src. It returns src unchanged when nothing applies.
func EditImports(filename, src, oldSrc string, add []string, prune bool) (string, error) {
	if len(add) == 0 && !prune {
		return src, nil
	}
	fset, f, err := parse(filename, src)
	if err != nil {
		return "", err
	}
	changed := false
	if prune && oldSrc != "" {
		if _, of, oerr := parse(filename, oldSrc); oerr == nil {
			var drop []*ast.ImportSpec
			for _, imp := range f.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				if imp.Name != nil && (imp.Name.Name == "_" || imp.Name.Name == ".") {
					continue
				}
				if astutil.UsesImport(of, p) && !astutil.UsesImport(f, p) {
					drop = append(drop, imp)
				}
			}
			for _, imp := range drop {
				p, _ := strconv.Unquote(imp.Path.Value)
				name := ""
				if imp.Name != nil {
					name = imp.Name.Name
				}
				if astutil.DeleteNamedImport(fset, f, name, p) {
					changed = true
				}
			}
		}
	}
	for _, spec := range add {
		alias, p, err := ParseImportSpec(spec)
		if err != nil {
			return "", err
		}
		if astutil.AddNamedImport(fset, f, alias, p) {
			changed = true
		}
	}
	if !changed {
		return src, nil
	}
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return "", toolerr.Inputf("cannot print the file after editing imports: %v", err)
	}
	return buf.String(), nil
}

// ImportsUsed lists, as `alias "path"` specs, the imports of src that the code in snippet uses.
func ImportsUsed(filename, src, snippet string) []string {
	_, f, err := parse(filename, src)
	if err != nil {
		return nil
	}
	type imp struct{ path, spec string }
	var imps []imp
	var header strings.Builder
	header.WriteString("package p\n")
	for _, i := range f.Imports {
		line := i.Path.Value
		if i.Name != nil {
			if i.Name.Name == "_" || i.Name.Name == "." {
				continue
			}
			line = i.Name.Name + " " + line
		}
		p, _ := strconv.Unquote(i.Path.Value)
		imps = append(imps, imp{p, line})
		header.WriteString("import " + line + "\n")
	}
	_, sf, err := parse(filename, header.String()+snippet)
	if err != nil {
		return nil
	}
	var out []string
	for _, i := range imps {
		if astutil.UsesImport(sf, i.path) {
			out = append(out, i.spec)
		}
	}
	return out
}
