// Package index is a lightweight workspace symbol index built with go/parser (declarations only).
//
// gopls's workspace/symbol is capped, fuzzy and limited to the current build configuration: files
// excluded by build tags, packages that are not built and brand-new directories can be missing or
// flaky. The index reads every .go file below the root itself, so it is authoritative for "which files
// declare X"; gopls stays the source of kinds and resolved positions where it has them.
//
// Files are re-parsed only when their mtime or size changed, so Refresh is a walk plus one stat per file.
package index

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
)

// LSP symbol kinds, as gopls reports them.
const (
	KindType      = 5
	KindMethod    = 6
	KindField     = 8
	KindInterface = 11
	KindFunction  = 12
	KindVariable  = 13
	KindConstant  = 14
	KindStruct    = 23
)

// maxFiles bounds how many files one index holds (a pathological checkout should not stall every call).
const maxFiles = 30000

// Entry is one declaration.
type Entry struct {
	Name string // declared name
	Recv string // owning type of a method or field; "" for package-level declarations
	Kind int    // LSP symbol kind
	File string // absolute path
	// Line and Col are the 0-based position of the name (Col in UTF-16 units).
	Line, Col int
	Dir       string // absolute package directory
	Pkg       string // package name
	// Constraint is why the file may not be part of a build: "//go:build linux", "// +build ignore" or
	// "file name suffix _windows"; "" for an unconditional file.
	Constraint string
	Test       bool // the file is a _test.go file
}

// Qual is the name gopls gives the symbol: "Name", or "Type.Name" for a method or field.
func (e Entry) Qual() string {
	if e.Recv != "" {
		return e.Recv + "." + e.Name
	}
	return e.Name
}

type fileRec struct {
	mtime, size int64
	entries     []Entry
}

// Index holds the declarations of the .go files below a root.
type Index struct {
	root string

	mu     sync.Mutex
	files  map[string]*fileRec
	byQual map[string][]*Entry // lower-case Qual -> entries
	all    []*Entry
}

// New returns an empty index for root; call Refresh to fill it.
func New(root string) *Index {
	return &Index{root: root, files: map[string]*fileRec{}, byQual: map[string][]*Entry{}}
}

// Root is the directory the index covers.
func (ix *Index) Root() string { return ix.root }

// skipDir: directories the go tool ignores or no tool should walk (.git, vendor, node_modules, testdata, _x, .x).
func skipDir(name string) bool {
	return exclude.IsDirName(name) || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// Refresh brings the index up to date with the disk: new and changed files are parsed, deleted ones dropped.
func (ix *Index) Refresh() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	seen := map[string]bool{}
	changed := false
	n := 0
	_ = filepath.WalkDir(ix.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != ix.root && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasPrefix(d.Name(), "_") || strings.HasPrefix(d.Name(), ".") {
			return nil // the go tool never compiles _x.go and .x.go
		}
		if n++; n > maxFiles {
			return filepath.SkipAll
		}
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			return nil
		}
		seen[p] = true
		if rec := ix.files[p]; rec != nil && rec.mtime == st.ModTime().UnixNano() && rec.size == st.Size() {
			return nil
		}
		ix.files[p] = &fileRec{mtime: st.ModTime().UnixNano(), size: st.Size(), entries: parseFile(p)}
		changed = true
		return nil
	})
	for p := range ix.files {
		if !seen[p] {
			delete(ix.files, p)
			changed = true
		}
	}
	if changed {
		ix.rebuild()
	}
	return nil
}

func (ix *Index) rebuild() {
	ix.byQual = map[string][]*Entry{}
	ix.all = ix.all[:0:0]
	paths := make([]string, 0, len(ix.files))
	for p := range ix.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		es := ix.files[p].entries
		for i := range es {
			k := strings.ToLower(es[i].Qual())
			ix.byQual[k] = append(ix.byQual[k], &es[i])
			ix.all = append(ix.all, &es[i])
		}
	}
}

// Len is the number of indexed declarations.
func (ix *Index) Len() int {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return len(ix.all)
}

// Lookup finds the declarations a name refers to. query is "Name", "Type.Member", "pkg.Name" or
// "pkg.Type.Member", where pkg is the package name or the base name of its directory. exact are the
// case-sensitive matches; loose the ones that differ only in case.
func (ix *Index) Lookup(query string) (exact, loose []Entry) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	type form struct{ qual, pkg string }
	forms := []form{{query, ""}}
	if i := strings.Index(query, "."); i > 0 && i < len(query)-1 {
		forms = append(forms, form{query[i+1:], query[:i]})
	}
	seen := map[*Entry]bool{}
	for _, f := range forms {
		for _, e := range ix.byQual[strings.ToLower(f.qual)] {
			if seen[e] {
				continue
			}
			pkgOK, pkgExact := true, true
			if f.pkg != "" {
				base := filepath.Base(e.Dir)
				pkgOK = strings.EqualFold(e.Pkg, f.pkg) || strings.EqualFold(base, f.pkg)
				pkgExact = e.Pkg == f.pkg || base == f.pkg
			}
			if !pkgOK {
				continue
			}
			seen[e] = true
			if e.Qual() == f.qual && pkgExact {
				exact = append(exact, *e)
			} else {
				loose = append(loose, *e)
			}
		}
	}
	return exact, loose
}

// Prefix lists the declarations whose name (or Type.Member name) starts with q, ignoring case; an exact
// name comes with them. Empty q matches nothing.
func (ix *Index) Prefix(q string) []Entry {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var out []Entry
	for _, e := range ix.all {
		if strings.HasPrefix(strings.ToLower(e.Name), q) || e.Recv != "" && strings.HasPrefix(strings.ToLower(e.Qual()), q) {
			out = append(out, *e)
		}
	}
	return out
}

var knownOS = setOf("aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "js", "linux", "nacl", "netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows", "zos")
var knownArch = setOf("386", "amd64", "amd64p32", "arm", "armbe", "arm64", "arm64be", "loong64", "mips", "mipsle", "mips64", "mips64le", "mips64p32", "mips64p32le", "ppc", "ppc64", "ppc64le", "riscv", "riscv64", "s390", "s390x", "sparc", "sparc64", "wasm")

func setOf(s ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range s {
		m[x] = true
	}
	return m
}

// nameConstraint is the GOOS/GOARCH suffix of a file name (foo_linux.go, foo_linux_arm64.go), "" if none.
func nameConstraint(file string) string {
	name := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(file), ".go"), "_test")
	parts := strings.Split(name, "_")
	if len(parts) < 2 {
		return ""
	}
	last := parts[len(parts)-1]
	if knownArch[last] {
		if len(parts) >= 3 && knownOS[parts[len(parts)-2]] {
			return "file name suffix _" + parts[len(parts)-2] + "_" + last
		}
		return "file name suffix _" + last
	}
	if knownOS[last] {
		return "file name suffix _" + last
	}
	return ""
}

// headerConstraint is the //go:build (or legacy // +build) line before the package clause.
func headerConstraint(f *ast.File) string {
	plus := ""
	for _, g := range f.Comments {
		if g.Pos() >= f.Package {
			break
		}
		for _, c := range g.List {
			switch {
			case constraint.IsGoBuild(c.Text):
				return strings.TrimSpace(c.Text)
			case constraint.IsPlusBuild(c.Text) && plus == "":
				plus = strings.TrimSpace(c.Text)
			}
		}
	}
	return plus
}

// Unbuildable reports whether the go tool never compiles the file whatever the build flags: a name
// starting with `_` or `.`, or a `//go:build ignore` header.
func Unbuildable(path string) bool {
	if unbuildableName(path) {
		return true
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return UnbuildableText(path, string(src))
}

func unbuildableName(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".")
}

// UnbuildableText is Unbuildable for the given content of path (an edit's result, not the disk).
func UnbuildableText(path, src string) bool {
	if unbuildableName(path) {
		return true
	}
	f, _ := parser.ParseFile(token.NewFileSet(), path, src, parser.PackageClauseOnly|parser.ParseComments)
	return f != nil && f.Name != nil && neverBuilt(headerConstraint(f))
}

// neverBuilt: the constraint needs the conventional "ignore" tag (//go:build ignore), which no build sets,
// whatever the other tags are. Such files are scratch programs and generators, not part of any package.
func neverBuilt(cons string) bool {
	if !constraint.IsGoBuild(cons) {
		return false
	}
	expr, err := constraint.Parse(cons)
	if err != nil {
		return false
	}
	for _, other := range []bool{true, false} {
		if expr.Eval(func(tag string) bool { return tag != "ignore" && other }) {
			return false
		}
	}
	return true
}

// utf16Col is the UTF-16 length of the line's text before byte offset off.
func utf16Col(src []byte, lineStart, off int) int {
	n := 0
	for i := lineStart; i < off && i < len(src); {
		r, size := utf8.DecodeRune(src[i:])
		n += len(utf16.Encode([]rune{r}))
		i += size
	}
	return n
}

// parseFile lists the declarations of one file (partial when it has syntax errors).
func parseFile(path string) []Entry {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	fset := token.NewFileSet()
	f, _ := parser.ParseFile(fset, path, src, parser.SkipObjectResolution|parser.ParseComments)
	if f == nil || f.Name == nil {
		return nil
	}
	tf := fset.File(f.Pos())
	cons := headerConstraint(f)
	if neverBuilt(cons) {
		return nil
	}
	if cons == "" {
		cons = nameConstraint(path)
	}
	dir := filepath.Dir(path)
	test := strings.HasSuffix(path, "_test.go")
	var out []Entry
	add := func(id *ast.Ident, recv string, kind int) {
		if id == nil || id.Name == "_" {
			return
		}
		off := tf.Offset(id.Pos())
		line := tf.Line(id.Pos())
		out = append(out, Entry{
			Name: id.Name, Recv: recv, Kind: kind, File: path,
			Line: line - 1, Col: utf16Col(src, tf.Offset(tf.LineStart(line)), off),
			Dir: dir, Pkg: f.Name.Name, Constraint: cons, Test: test,
		})
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) > 0 {
				add(d.Name, recvName(d.Recv.List[0].Type), KindMethod)
			} else {
				add(d.Name, "", KindFunction)
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					kind := KindType
					switch s.Type.(type) {
					case *ast.StructType:
						kind = KindStruct
					case *ast.InterfaceType:
						kind = KindInterface
					}
					add(s.Name, "", kind)
					switch t := s.Type.(type) {
					case *ast.StructType:
						for _, fl := range t.Fields.List {
							if len(fl.Names) == 0 {
								add(embeddedName(fl.Type), s.Name.Name, KindField)
							}
							for _, n := range fl.Names {
								add(n, s.Name.Name, KindField)
							}
						}
					case *ast.InterfaceType:
						for _, m := range t.Methods.List {
							for _, n := range m.Names {
								add(n, s.Name.Name, KindMethod)
							}
						}
					}
				case *ast.ValueSpec:
					kind := KindVariable
					if d.Tok == token.CONST {
						kind = KindConstant
					}
					for _, n := range s.Names {
						add(n, "", kind)
					}
				}
			}
		}
	}
	return out
}

// recvName is the type name of a method receiver: T for T, *T, T[P] and *T[P].
func recvName(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}

// embeddedName is the identifier naming an embedded field: T for T, *T, pkg.T and T[P].
func embeddedName(e ast.Expr) *ast.Ident {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.SelectorExpr:
			return t.Sel
		case *ast.Ident:
			return t
		default:
			return nil
		}
	}
}
