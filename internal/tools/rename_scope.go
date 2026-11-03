package tools

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// receiverSpan is the byte span of the method whose receiver name is at off in src (ok is false when off is not
// the name of a receiver).
func receiverSpan(path, src string, off int) (start, end int, ok bool) {
	fset := token.NewFileSet()
	f, _ := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if f == nil {
		return 0, 0, false
	}
	for _, d := range f.Decls {
		fd, isFunc := d.(*ast.FuncDecl)
		if !isFunc || fd.Recv == nil {
			continue
		}
		for _, fld := range fd.Recv.List {
			for _, id := range fld.Names {
				if fset.Position(id.Pos()).Offset == off {
					return fset.Position(fd.Pos()).Offset, fset.Position(fd.End()).Offset, true
				}
			}
		}
	}
	return 0, 0, false
}

// limitToReceiver: a receiver is visible only inside its method. When the renamed name is a receiver, its edits
// lie in that method's declaration; gopls also renames the receivers of the type's other methods (REN-001), which
// are other objects, so every edit outside the declaration is dropped. It returns how many were dropped.
func limitToReceiver(decl member, by map[string][]lsp.TextEdit) int {
	text, _, err := edits.ReadSource(decl.path)
	if err != nil {
		return 0
	}
	a, b, ok := receiverSpan(decl.path, text, edits.Offset(text, decl.line, decl.col))
	if !ok {
		return 0
	}
	dropped := 0
	for p, es := range by {
		var keep []lsp.TextEdit
		for _, e := range es {
			if p == decl.path {
				if off := edits.Offset(text, e.Range.Start.Line, e.Range.Start.Character); off >= a && off < b {
					keep = append(keep, e)
					continue
				}
			}
			dropped++
		}
		if len(keep) == 0 {
			delete(by, p)
		} else {
			by[p] = keep
		}
	}
	return dropped
}

// dropForeignQualified removes the edits to an identifier that is qualified by another package's name: `sub.B`
// (an embedded alias of another package's type, renamed as a field: REN-004) or a doc link `[cmp.Ordered]` (BED-003).
// The renamed object lives in decl's package, so an identifier a file reaches through an import of another package
// is another object, and so is a doc-link qualifier that names a package the file does not import. A qualifier
// that is not a package (a variable's field, a type's method in a doc link) is left alone. It returns the dropped
// occurrences as "file:line (text)".
func dropForeignQualified(w *Workspace, decl member, by map[string][]lsp.TextEdit) []string {
	dir := filepath.Dir(decl.path)
	m, err := moduleOf(dir)
	if err != nil {
		return nil
	}
	target, err := importPathOf(m, dir)
	if err != nil || target == "" {
		return nil
	}
	declText, _, err := edits.ReadSource(decl.path)
	if err != nil {
		return nil
	}
	targetName := gosrc.PackageName(declText)
	var dropped []string
	packages := map[string]bool{}
	for p, es := range by {
		if !strings.HasSuffix(p, ".go") {
			continue
		}
		text, _, err := edits.ReadSource(p)
		if err != nil {
			continue
		}
		fset := token.NewFileSet()
		f, _ := parser.ParseFile(fset, p, text, parser.ParseComments)
		if f == nil {
			continue
		}
		imports := map[string]string{}
		for _, is := range f.Imports {
			ip, err := strconv.Unquote(is.Path.Value)
			if err != nil {
				continue
			}
			name := gosrc.ImportName(ip)
			if is.Name != nil {
				if is.Name.Name == "_" || is.Name.Name == "." {
					continue
				}
				name = is.Name.Name
			}
			imports[name] = ip
		}
		var keep []lsp.TextEdit
		for _, e := range es {
			off := edits.Offset(text, e.Range.Start.Line, e.Range.Start.Character)
			end := edits.Offset(text, e.Range.End.Line, e.Range.End.Character)
			q, from, ok := qualifierOf(text, off, end)
			if !ok {
				keep = append(keep, e)
				continue
			}
			if id := identAtOffset(fset, f, from); id != nil && id.Obj != nil {
				// A local or a parameter of that name shadows the import (url shadows net/url): not a package.
				keep = append(keep, e)
				continue
			}
			foreign := false
			if ip, isImport := imports[q]; isImport {
				foreign = ip != target
			} else if q != targetName && inCommentAt(fset, f, off) {
				foreign = namesPackage(w.Root, q, packages)
			}
			if !foreign {
				keep = append(keep, e)
				continue
			}
			dropped = append(dropped, fmt.Sprintf("%s:%d (%s)", edits.RelativeName(p, w.Root), e.Range.Start.Line+1, text[from:end]))
		}
		if len(keep) == 0 {
			delete(by, p)
		} else {
			by[p] = keep
		}
	}
	return dropped
}

// qualifierOf is the package qualifier of the identifier at [off,end) of text ("sub" in sub.B), whether the edit
// spans the qualified name or only its last part. from is where the qualified name starts.
func qualifierOf(text string, off, end int) (q string, from int, ok bool) {
	if s := text[off:end]; strings.Contains(s, ".") {
		q := s[:strings.LastIndex(s, ".")]
		if q == "" || !isIdentText(q) {
			return "", 0, false
		}
		return q, off, true
	}
	if off >= 2 && text[off-1] == '.' {
		k := off - 1
		for k > 0 && isIdentByte(text[k-1]) {
			k--
		}
		if k < off-1 {
			return text[k : off-1], k, true
		}
	}
	return "", 0, false
}

func isIdentByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}

func isIdentText(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isIdentByte(s[i]) {
			return false
		}
	}
	return s != ""
}

// inCommentAt reports whether byte offset off of the parsed file lies inside a comment.
func inCommentAt(fset *token.FileSet, f *ast.File, off int) bool {
	for _, c := range f.Comments {
		if fset.Position(c.Pos()).Offset <= off && off < fset.Position(c.End()).Offset {
			return true
		}
	}
	return false
}

// namesPackage reports whether q is the name of a package: a standard-library one, or a package directory of
// the workspace. memo caches the answer per call.
func namesPackage(root, q string, memo map[string]bool) bool {
	if v, ok := memo[q]; ok {
		return v
	}
	spec, amb := gosrc.StdlibImport(q)
	v := spec != "" || amb || packageDir(root, q) != ""
	memo[q] = v
	return v
}

// unchangedNote says which occurrences the rename left as they are, and why.
func unchangedNote(labels []string, name string) string {
	const shown = 5
	list, more := labels, ""
	if len(list) > shown {
		more = fmt.Sprintf(" and %d more", len(list)-shown)
		list = list[:shown]
	}
	return fmt.Sprintf("%d occurrence(s) of %s are left unchanged: each is qualified by another package's name, so it is not this symbol (%s%s).",
		len(labels), name, strings.Join(list, "; "), more)
}

// identAtOffset is the identifier that starts at byte offset off of the parsed file (nil when none does).
func identAtOffset(fset *token.FileSet, f *ast.File, off int) *ast.Ident {
	var found *ast.Ident
	ast.Inspect(f, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		if id, ok := n.(*ast.Ident); ok && fset.Position(id.Pos()).Offset == off {
			found = id
		}
		return found == nil
	})
	return found
}

// foreignImplementer labels the type outside the workspace whose method is at m: its package and type name
// ("assert.Assertions", "fs.FileInfo"). gopls lists two kinds of place for an interface method's implementers: a
// method with a receiver, and a method spec of another interface (fs.FileInfo.Name), which has no receiver. When
// the position is neither, the label is the package and the line, never a path.
func foreignImplementer(m member) string {
	text, _, err := edits.ReadSource(m.path)
	if err != nil {
		return "a type outside the workspace"
	}
	pkg := gosrc.PackageName(text)
	fallback := fmt.Sprintf("a type in package %s (line %d)", pkg, m.line+1)
	fset := token.NewFileSet()
	f, _ := parser.ParseFile(fset, m.path, text, parser.SkipObjectResolution)
	if f == nil {
		return fallback
	}
	off := edits.Offset(text, m.line, m.col)
	within := func(n ast.Node) bool {
		return fset.Position(n.Pos()).Offset <= off && off < fset.Position(n.End()).Offset
	}
	label := ""
	ast.Inspect(f, func(n ast.Node) bool {
		if label != "" {
			return false
		}
		switch n := n.(type) {
		case *ast.FuncDecl:
			if n.Recv != nil && len(n.Recv.List) > 0 && within(n) {
				if t := recvTypeName(n.Recv.List[0].Type); t != "" {
					label = pkg + "." + t
				}
			}
		case *ast.TypeSpec:
			it, ok := n.Type.(*ast.InterfaceType)
			if !ok || !within(n) {
				break
			}
			for _, fld := range it.Methods.List {
				for _, id := range fld.Names {
					if fset.Position(id.Pos()).Offset == off {
						label = pkg + "." + n.Name.Name
					}
				}
			}
		}
		return label == ""
	})
	if label == "" {
		return fallback
	}
	return label
}
