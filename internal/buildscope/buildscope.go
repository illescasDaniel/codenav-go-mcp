// Package buildscope tells which Go files the build leaves out, so a tool can say that a use or a
// declaration sits outside the build it checks, instead of changing it silently.
//
// gopls and the server's type check see one build: the GOOS, GOARCH, cgo setting and tags of Context.
// The go tool leaves other files out (a _windows suffix, a //go:build line that is false here), and
// neither gopls references nor the type check see them, so a write can leave them broken.
//
// The API, all of it:
//
//	Context() *build.Context          build.Default (GOOS, GOARCH from the environment) plus the -tags of
//	                                  CODENAV_GO_MCP_BUILD_FLAGS (-tags=a,b or -tags a,b)
//	Excluded(c, path) (why, bool)     whether the go tool leaves path out of c's build, and why ("file name
//	                                  suffix _windows", "//go:build integration")
//	Mentions(c, Query) []Hit          the uses, declarations and member selectors of a name in the files that
//	                                  c's build leaves out and could refer to the object declared in DeclFile
//	NameConstraint(file) string       the GOOS/GOARCH suffix of a file name as a reason, "" if none
//	NeverBuilt(cons) bool             a //go:build line no build satisfies (the "ignore" tag): scratch programs
//
// Mentions returns one Hit per mention: its Path, 1-based Line and Col (bytes), its Kind, and the Reason
// its file is left out of the build. Kinds:
//
//   - Use: a bare reference to the package-level name in the declaring package, or pkg.Name in a file of
//     another package that imports ImportPath (exported names only). A local that shadows the name, a
//     struct field, a method name, a label, and a selector x.name are not uses.
//   - Decl: a top-level func, type, var or const of the name in the declaring package.
//   - Member: when Query.Member is set (the name is a method or field): every selector of the name and
//     every member declaration of it in the scanned files, whatever the receiver type.
//   - Asm: a `TEXT ·name(SB)` line in a .s file of the declaring directory. Asm hits are reported whether
//     the file is built or not: Reason is "" when it is built.
//
// Use, Decl and Member hits come only from excluded .go files; a file gopls sees is not reported, because
// its mentions are the references gopls already gives. Files the go tool never reads are skipped: a name
// starting with _ or ., and a //go:build line NeverBuilt accepts as unsatisfiable. Matching works on the
// parsed syntax, so comments and strings never match, and a bare name is resolved against the parser's
// file scope, so a shadowing local is not reported. The files read are those of the declaring directory
// and, for exported names with an ImportPath, the files below Root that import it.
package buildscope

import (
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
)

// Context is the build the server checks: build.Default with the -tags of CODENAV_GO_MCP_BUILD_FLAGS added.
// Each call returns a fresh copy.
func Context() *build.Context {
	c := build.Default
	c.BuildTags = append(append([]string(nil), c.BuildTags...), tagsOf(os.Getenv(gomod.EnvBuildFlags))...)
	return &c
}

func tagsOf(flags string) []string {
	var tags []string
	f := strings.Fields(flags)
	for i := 0; i < len(f); i++ {
		v, ok := "", false
		switch {
		case f[i] == "-tags" || f[i] == "--tags":
			if i+1 < len(f) {
				i++
				v, ok = f[i], true
			}
		case strings.HasPrefix(f[i], "-tags="):
			v, ok = strings.TrimPrefix(f[i], "-tags="), true
		case strings.HasPrefix(f[i], "--tags="):
			v, ok = strings.TrimPrefix(f[i], "--tags="), true
		}
		if !ok {
			continue
		}
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tags = append(tags, t)
			}
		}
	}
	return tags
}

// Excluded reports whether the go tool leaves path out of the build of c, and why. Files the go tool never
// reads (a name starting with _ or .) count as excluded too. A file that cannot be read is not excluded.
func Excluded(c *build.Context, path string) (string, bool) {
	ok, err := c.MatchFile(filepath.Dir(path), filepath.Base(path))
	if err != nil || ok {
		return "", false
	}
	name := filepath.Base(path)
	if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
		return "its name starts with _ or . (the go tool ignores it)", true
	}
	if goos, goarch := suffixParts(name); !suffixMatches(c, goos, goarch) {
		return NameConstraint(name), true
	}
	src, rerr := readFile(c, path)
	if rerr != nil {
		return "build constraints", true
	}
	if cons := headerLine(src); cons != "" {
		return cons, true
	}
	return "build constraints", true
}

// NameConstraint is the GOOS/GOARCH suffix of a file name as a reason ("file name suffix _windows",
// "file name suffix _linux_amd64"), "" when the name has none.
func NameConstraint(file string) string {
	goos, goarch := suffixParts(filepath.Base(file))
	switch {
	case goos != "" && goarch != "":
		return "file name suffix _" + goos + "_" + goarch
	case goos != "":
		return "file name suffix _" + goos
	case goarch != "":
		return "file name suffix _" + goarch
	}
	return ""
}

// NeverBuilt reports whether the constraint line cons is one no build satisfies: it is false whatever the
// other tags are, because it needs the conventional "ignore" tag, which no build sets (scratch programs and
// generators, which go run builds on their own).
func NeverBuilt(cons string) bool {
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

var knownOS = setOf("aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "js", "linux", "nacl", "netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows", "zos")
var knownArch = setOf("386", "amd64", "amd64p32", "arm", "armbe", "arm64", "arm64be", "loong64", "mips", "mipsle", "mips64", "mips64le", "mips64p32", "mips64p32le", "ppc", "ppc64", "ppc64le", "riscv", "riscv64", "s390", "s390x", "sparc", "sparc64", "wasm")

func setOf(s ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range s {
		m[x] = true
	}
	return m
}

// suffixParts reads the GOOS and GOARCH a file name constrains, the way go/build does: the part from the
// first "_" of the name (before its first "."), with a trailing "_test" ignored.
func suffixParts(name string) (goos, goarch string) {
	base, _, _ := strings.Cut(name, ".")
	i := strings.Index(base, "_")
	if i < 0 {
		return "", ""
	}
	l := strings.Split(base[i:], "_")
	if n := len(l); l[n-1] == "test" {
		l = l[:n-1]
	}
	n := len(l)
	switch {
	case n >= 3 && knownOS[l[n-2]] && knownArch[l[n-1]]:
		return l[n-2], l[n-1]
	case n >= 2 && knownOS[l[n-1]]:
		return l[n-1], ""
	case n >= 2 && knownArch[l[n-1]]:
		return "", l[n-1]
	}
	return "", ""
}

// suffixMatches reports whether the GOOS and GOARCH of a file name fit c (an empty one always fits).
func suffixMatches(c *build.Context, goos, goarch string) bool {
	osOK := goos == "" || goos == c.GOOS ||
		goos == "linux" && c.GOOS == "android" || goos == "darwin" && c.GOOS == "ios" || goos == "solaris" && c.GOOS == "illumos"
	return osOK && (goarch == "" || goarch == c.GOARCH)
}

// headerLine is the //go:build (or legacy // +build) line before the package clause, "" if none.
func headerLine(src string) string {
	plus := ""
	for rest := src; rest != ""; {
		var line string
		line, rest, _ = strings.Cut(rest, "\n")
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "package ") {
			break
		}
		switch {
		case constraint.IsGoBuild(t):
			return t
		case constraint.IsPlusBuild(t) && plus == "":
			plus = t
		}
	}
	return plus
}

// Query selects the mentions Mentions looks for.
type Query struct {
	Root       string            // workspace root: the files that import ImportPath are searched below it
	DeclFile   string            // a file of the declaring package (absolute path)
	Name       string            // the identifier
	Member     bool              // the name is a method or field: Member hits instead of Use and Decl
	ImportPath string            // the declaring package's import path; "" = do not search other packages
	Texts      map[string]string // contents that replace files on disk (an edit's result), by absolute path
}

// Kind is what a Hit mentions.
type Kind int

const (
	Use    Kind = iota // a reference to the package-level name
	Decl               // a top-level declaration of the name
	Member             // a selector or member declaration of the name (Query.Member)
	Asm                // a TEXT ·name(SB) line in a .s file of the declaring directory
)

// Hit is one mention of the name in a file the build leaves out (see the package comment).
type Hit struct {
	Path   string
	Line   int    // 1-based
	Col    int    // 1-based, in bytes
	Kind   Kind   // what is mentioned
	Reason string // why the file is left out of the build; "" for an Asm hit in a built file
}

// Mentions lists the mentions of q.Name that the build of c leaves out (see the package comment).
// The declaring directory is read first; then, for exported names with an ImportPath, the files below Root
// that import it.
func Mentions(c *build.Context, q Query) []Hit {
	if q.Name == "" || q.Name == "_" {
		return nil
	}
	cc := withTexts(c, q.Texts)
	dir := filepath.Dir(q.DeclFile)
	declPkg := packageName(cc, q.DeclFile)
	var out []Hit
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		switch filepath.Ext(p) {
		case ".go":
			out = append(out, scanGo(cc, q, p, declPkg, true)...)
		case ".s":
			if !q.Member {
				out = append(out, scanAsm(cc, q, p)...)
			}
		}
	}
	if q.ImportPath != "" && token.IsExported(q.Name) && q.Root != "" {
		_ = filepath.WalkDir(q.Root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != q.Root && (exclude.IsDirName(d.Name()) || d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_")) {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(p, ".go") && filepath.Dir(p) != dir {
				out = append(out, scanGo(cc, q, p, declPkg, false)...)
			}
			return nil
		})
	}
	return out
}

// withTexts is c with OpenFile reading the given contents first (the go tool reads build headers through it).
func withTexts(c *build.Context, texts map[string]string) *build.Context {
	cc := *c
	open := c.OpenFile
	cc.OpenFile = func(path string) (io.ReadCloser, error) {
		if t, ok := texts[path]; ok {
			return io.NopCloser(strings.NewReader(t)), nil
		}
		if open != nil {
			return open(path)
		}
		return os.Open(path)
	}
	return &cc
}

// readFile is the content of path as c reads it.
func readFile(c *build.Context, path string) (string, error) {
	if c.OpenFile == nil {
		b, err := os.ReadFile(path)
		return string(b), err
	}
	rc, err := c.OpenFile(path)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return string(b), err
}

// packageName is the package clause name of path ("" if unreadable).
func packageName(c *build.Context, path string) string {
	src, err := readFile(c, path)
	if err != nil {
		return ""
	}
	f, _ := parser.ParseFile(token.NewFileSet(), path, src, parser.PackageClauseOnly)
	if f == nil || f.Name == nil {
		return ""
	}
	return f.Name.Name
}

// scanGo reports the mentions in one excluded .go file. A file of the declaring directory in the declaring
// package is read as that package; any other file is read through its import of ImportPath.
func scanGo(c *build.Context, q Query, path, declPkg string, sameDir bool) []Hit {
	src, err := readFile(c, path)
	if err != nil || !strings.Contains(src, q.Name) {
		return nil
	}
	base := filepath.Base(path)
	if strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") || NeverBuilt(headerLine(src)) {
		return nil
	}
	reason, excluded := Excluded(c, path)
	if !excluded {
		return nil // gopls and the type check see this file
	}
	fset := token.NewFileSet()
	f, _ := parser.ParseFile(fset, path, src, 0)
	if f == nil || f.Name == nil {
		return nil
	}
	mk := func(id *ast.Ident, k Kind) Hit {
		p := fset.PositionFor(id.Pos(), false)
		return Hit{Path: path, Line: p.Line, Col: p.Column, Kind: k, Reason: reason}
	}
	if sameDir && (declPkg == "" || f.Name.Name == declPkg) {
		return packageHits(f, q.Name, q.Member, true, mk)
	}
	if q.ImportPath == "" || !token.IsExported(q.Name) {
		return nil
	}
	local, ok := importName(f, q.ImportPath, declPkg)
	if !ok {
		return nil
	}
	if local == "." {
		return packageHits(f, q.Name, q.Member, false, mk)
	}
	return qualifiedHits(f, q.Name, local, q.Member, mk)
}

// importName is the name under which f imports importPath ("." for a dot import); ok is false when f does
// not import it, or imports it as _.
func importName(f *ast.File, importPath, declPkg string) (string, bool) {
	for _, s := range f.Imports {
		p, err := strconv.Unquote(s.Path.Value)
		if err != nil || p != importPath {
			continue
		}
		if s.Name != nil {
			if s.Name.Name == "_" {
				return "", false
			}
			return s.Name.Name, true
		}
		return declPkg, declPkg != ""
	}
	return "", false
}

// Roles of the identifiers named like the query, by their place in the syntax.
const (
	plainIdent  = iota // a bare identifier: a use unless a local shadows it
	selectorSel        // the Sel of x.name: a field or method
	memberDecl         // a method name, or a field of a struct or interface
	keyLit             // a composite literal key: a field, or a variable of a map literal
	labelIdent         // a label (not a package-level name)
	importAlias        // an import alias (not a package-level name)
	topDecl            // a top-level func, type, var or const
)

// packageHits finds the mentions of name in a file of its own package. decls says whether the top-level
// declarations of name count (they do for the package itself, not for a dot import).
func packageHits(f *ast.File, name string, member, decls bool, mk func(*ast.Ident, Kind) Hit) []Hit {
	role := map[*ast.Ident]int{}
	mark := func(id *ast.Ident, r int) {
		if id != nil && id.Name == name {
			role[id] = r
		}
	}
	markFields := func(l *ast.FieldList) {
		if l == nil {
			return
		}
		for _, fl := range l.List {
			for _, id := range fl.Names {
				mark(id, memberDecl)
			}
		}
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil {
				mark(d.Name, memberDecl)
			} else {
				mark(d.Name, topDecl)
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					mark(s.Name, topDecl)
				case *ast.ValueSpec:
					for _, id := range s.Names {
						mark(id, topDecl)
					}
				}
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			mark(n.Sel, selectorSel)
		case *ast.StructType:
			markFields(n.Fields)
		case *ast.InterfaceType:
			markFields(n.Methods)
		case *ast.KeyValueExpr:
			if id, ok := n.Key.(*ast.Ident); ok {
				mark(id, keyLit)
			}
		case *ast.LabeledStmt:
			mark(n.Label, labelIdent)
		case *ast.BranchStmt:
			mark(n.Label, labelIdent)
		case *ast.ImportSpec:
			mark(n.Name, importAlias)
		}
		return true
	})
	var out []Hit
	ast.Inspect(f, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != name {
			return true
		}
		switch r := role[id]; {
		case member:
			if r == selectorSel || r == memberDecl || r == keyLit {
				out = append(out, mk(id, Member))
			}
		case r == topDecl:
			if decls {
				out = append(out, mk(id, Decl))
			}
		case r == plainIdent:
			// A local (parameter, variable, type parameter) shadows the package-level name; the parser's
			// file scope holds the package-level objects of this file, and an unresolved name is another file's.
			if id.Obj == nil || f.Scope != nil && f.Scope.Lookup(name) == id.Obj {
				out = append(out, mk(id, Use))
			}
		case r == keyLit:
			out = append(out, mk(id, Use))
		}
		return true
	})
	return out
}

// qualifiedHits finds local.name (the declaring package imported under the name local) and, for a member,
// every selector of the name.
func qualifiedHits(f *ast.File, name, local string, member bool, mk func(*ast.Ident, Kind) Hit) []Hit {
	var out []Hit
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return true
		}
		if member {
			out = append(out, mk(sel.Sel, Member))
		} else if x, ok := sel.X.(*ast.Ident); ok && x.Name == local && x.Obj == nil {
			out = append(out, mk(sel.Sel, Use))
		}
		return true
	})
	return out
}

var asmTextRe = regexp.MustCompile(`(?m)^[ \t]*TEXT[ \t]+[^ \t·]*·([\p{L}_][\p{L}\p{N}_]*)\(SB\)`)

// scanAsm reports the TEXT ·name(SB) lines of one .s file.
func scanAsm(c *build.Context, q Query, path string) []Hit {
	src, err := readFile(c, path)
	if err != nil || !strings.Contains(src, q.Name) {
		return nil
	}
	reason, _ := Excluded(c, path) // "" when the file is built
	var out []Hit
	for _, m := range asmTextRe.FindAllStringSubmatchIndex(src, -1) {
		if src[m[2]:m[3]] != q.Name {
			continue
		}
		line := strings.Count(src[:m[2]], "\n") + 1
		col := m[2] - strings.LastIndex(src[:m[2]], "\n")
		out = append(out, Hit{Path: path, Line: line, Col: col, Kind: Asm, Reason: reason})
	}
	return out
}
