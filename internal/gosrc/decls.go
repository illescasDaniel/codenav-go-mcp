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
	Recv       string // receiver type name of a method
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
			recv := ""
			if d.Recv != nil && len(d.Recv.List) == 1 {
				recv = recvName(d.Recv.List[0].Type)
			}
			return &Decl{Name: d.Name.Name, Keyword: "func", Method: d.Recv != nil, Recv: recv, Start: s, End: at(d.End()), HasDoc: doc, Closing: -1}, nil
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
						if trailing != nil && at(trailing.End()) > out.End {
							out.End = at(trailing.End())
						}
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

// FindDeclByName returns the top-level declaration called key (Name, or Recv.Name for a method) and
// the byte offset of its name; it needs no type information, so it also finds code that is excluded
// from the current build.
func FindDeclByName(filename, src, key string) (*Decl, int, error) {
	fset, f, err := parse(filename, src)
	if err != nil {
		return nil, 0, err
	}
	for _, decl := range f.Decls {
		var idents []*ast.Ident
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) == 1 {
				name = recvName(d.Recv.List[0].Type) + "." + name
			}
			if name == key {
				idents = append(idents, d.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					if sp.Name.Name == key {
						idents = append(idents, sp.Name)
					}
				case *ast.ValueSpec:
					for _, n := range sp.Names {
						if n.Name == key {
							idents = append(idents, n)
						}
					}
				}
			}
		}
		if len(idents) > 0 {
			off := fset.Position(idents[0].Pos()).Offset
			d, err := FindDeclAt(filename, src, off)
			return d, off, err
		}
	}
	return nil, 0, toolerr.Inputf("%s is not declared in %s", key, filename)
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

// SplitFile splits a complete source file into its import specs (`"os"`, `str "strings"`) and
// the text after the package clause and the import declarations.
func SplitFile(filename, src string) (imports []string, body string, err error) {
	fset, f, err := parse(filename, src)
	if err != nil {
		return nil, "", err
	}
	end := fset.Position(f.Name.End()).Offset
	for _, decl := range f.Decls {
		g, ok := decl.(*ast.GenDecl)
		if !ok || g.Tok != token.IMPORT {
			break
		}
		for _, sp := range g.Specs {
			is := sp.(*ast.ImportSpec)
			spec := is.Path.Value
			if is.Name != nil {
				spec = is.Name.Name + " " + spec
			}
			imports = append(imports, spec)
		}
		end = fset.Position(g.End()).Offset
	}
	return imports, strings.Trim(src[end:], "\r\n"), nil
}

// PackageNameRange returns the byte range of the identifier in the package clause (ok=false when src does not parse that far).
func PackageNameRange(src string) (start, end int, ok bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, parser.PackageClauseOnly)
	if err != nil || f.Name == nil {
		return 0, 0, false
	}
	return fset.Position(f.Name.Pos()).Offset, fset.Position(f.Name.End()).Offset, true
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
	collapseSingleImports(f)
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

// RewriteImports replaces import paths for which fn returns (new, true); the rest of the text is untouched.
func RewriteImports(filename, src string, fn func(path string) (string, bool)) (string, bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ImportsOnly)
	if err != nil {
		return src, false, toolerr.Inputf("not valid Go: %v", err)
	}
	var b strings.Builder
	pos, changed := 0, false
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		np, ok := fn(p)
		if !ok {
			continue
		}
		start, end := fset.Position(imp.Path.Pos()).Offset, fset.Position(imp.Path.End()).Offset
		b.WriteString(src[pos:start])
		b.WriteString(strconv.Quote(np))
		pos, changed = end, true
	}
	b.WriteString(src[pos:])
	return b.String(), changed, nil
}

// CarveSpec rewrites src so that the const/var/type whose name starts at nameOff stands alone in its own
// declaration (after the original one) instead of sharing a block or a `var a, b = ...` line. It returns
// the new text and the new offset of the name. Cases whose meaning would change are refused.
func CarveSpec(filename, src string, nameOff int) (string, int, error) {
	fset, f, err := parse(filename, src)
	if err != nil {
		return "", 0, err
	}
	at := func(p token.Pos) int { return fset.Position(p).Offset }
	lineStart := func(o int) int { return strings.LastIndex(src[:o], "\n") + 1 }
	lineEnd := func(o int) int {
		if i := strings.Index(src[o:], "\n"); i >= 0 {
			return o + i + 1
		}
		return len(src)
	}
	text := func(a, b token.Pos) string { return src[at(a):at(b)] }
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok == token.IMPORT {
			continue
		}
		for si, spec := range gd.Specs {
			var names []*ast.Ident
			var doc, trailing *ast.CommentGroup
			var vs *ast.ValueSpec
			switch s := spec.(type) {
			case *ast.TypeSpec:
				names, doc, trailing = []*ast.Ident{s.Name}, s.Doc, s.Comment
			case *ast.ValueSpec:
				vs, names, doc, trailing = s, s.Names, s.Doc, s.Comment
			}
			idx := -1
			for i, n := range names {
				if at(n.Pos()) == nameOff {
					idx = i
				}
			}
			if idx < 0 {
				continue
			}
			kw := gd.Tok.String()
			grouped := gd.Lparen.IsValid() && len(gd.Specs) > 1
			multi := len(names) > 1
			if multi && vs != nil && len(vs.Values) == 1 {
				return "", 0, toolerr.Inputf("%s is initialised by one multi-value expression together with other names; splitting it is not supported. Rewrite it with edit first.", names[idx].Name)
			}
			if gd.Tok == token.CONST && grouped {
				iota := false
				ast.Inspect(gd, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok && id.Name == "iota" {
						iota = true
					}
					return !iota
				})
				implicit := len(vs.Values) == 0
				if si+1 < len(gd.Specs) {
					implicit = implicit || len(gd.Specs[si+1].(*ast.ValueSpec).Values) == 0
				}
				if iota || implicit {
					return "", 0, toolerr.Inputf("%s is in a const block that uses iota or repeated values; removing it would change the values of the constants. Rewrite the block with explicit values first (edit).", names[idx].Name)
				}
			}
			// The standalone declaration.
			var body string
			switch {
			case vs == nil:
				body = text(spec.Pos(), spec.End())
			default:
				body = names[idx].Name
				if vs.Type != nil {
					body += " " + text(vs.Type.Pos(), vs.Type.End())
				}
				switch {
				case len(vs.Values) == len(names):
					body += " = " + text(vs.Values[idx].Pos(), vs.Values[idx].End())
				case len(vs.Values) > 0:
					return "", 0, toolerr.Inputf("cannot split %s from its declaration.", names[idx].Name)
				}
			}
			head := ""
			if !multi {
				if doc != nil {
					head = text(doc.Pos(), doc.End()) + "\n"
				}
				if trailing != nil {
					body += " " + text(trailing.Pos(), trailing.End())
				}
			}
			standalone := head + kw + " " + body
			nameRel := len(head) + len(kw) + 1

			var a, b int // replaced range
			var repl string
			if multi {
				var ns, vals []string
				for i, n := range names {
					if i == idx {
						continue
					}
					ns = append(ns, n.Name)
					if vs != nil && len(vs.Values) > 0 {
						vals = append(vals, text(vs.Values[i].Pos(), vs.Values[i].End()))
					}
				}
				repl = strings.Join(ns, ", ")
				if vs != nil && vs.Type != nil {
					repl += " " + text(vs.Type.Pos(), vs.Type.End())
				}
				if len(vals) > 0 {
					repl += " = " + strings.Join(vals, ", ")
				}
				if vs == nil {
					return "", 0, toolerr.Inputf("unsupported declaration.")
				}
				a, b = at(spec.Pos()), at(spec.End())
			} else {
				first, last := at(spec.Pos()), at(spec.End())
				if doc != nil {
					first = at(doc.Pos())
				}
				if trailing != nil {
					last = at(trailing.End())
				}
				if strings.TrimSpace(src[lineStart(first):first]) != "" || strings.TrimSpace(src[last:lineEnd(last)]) != "" {
					return "", 0, toolerr.Inputf("%s shares its line with other code; put each name of the block on its own line first (edit).", names[idx].Name)
				}
				a, b = lineStart(first), lineEnd(last)
			}
			ins := at(gd.End())
			if ins < b {
				return "", 0, toolerr.Inputf("unsupported declaration layout.")
			}
			out := src[:a] + repl + src[b:ins] + "\n\n" + standalone + src[ins:]
			return out, a + len(repl) + (ins - b) + 2 + nameRel, nil
		}
	}
	return "", 0, toolerr.Inputf("that symbol is not a const, var or type.")
}

// collapseSingleImports turns `import (\n\t"x"\n)` into `import "x"` (comment-free specs only).
func collapseSingleImports(f *ast.File) {
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.IMPORT || !g.Lparen.IsValid() || len(g.Specs) != 1 {
			continue
		}
		s := g.Specs[0].(*ast.ImportSpec)
		if s.Doc != nil || s.Comment != nil || g.Doc != nil {
			continue
		}
		if len(f.Comments) > 0 {
			inside := false
			for _, c := range f.Comments {
				if c.Pos() > g.Lparen && c.End() < g.Rparen {
					inside = true
				}
			}
			if inside {
				continue
			}
		}
		g.Lparen, g.Rparen = token.NoPos, token.NoPos
	}
}
