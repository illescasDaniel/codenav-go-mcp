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
//	ExcludedText(c, path, text)       Excluded for a file whose content is text (an edit's result, not on disk yet)
//	Mentions(c, Query) []Hit          the uses, declarations and member selectors of a name in the files that
//	                                  c's build leaves out and could refer to the object declared in DeclFile
//	MentionsAll(ctx, c, root, texts,  Mentions for many names of many declaring files: one walk of root in all
//	  []Target) (Result, error)       (Result.Walks), and it stops when ctx is done
//	NameConstraint(file) string       the GOOS/GOARCH suffix of a file name as a reason, "" if none
//	FileConstraint(path, src) string  the //go:build line that the name and header of a file put on it, normalized
//	                                  ("" if none): two files with the same one are built under the same conditions
//	Exclusive(a, b) bool              whether no build satisfies both constraints: such files never clash
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
	"context"
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
	"sort"
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

// ExcludedText is Excluded for a file whose content is text (the result of an edit, not yet on disk).
func ExcludedText(c *build.Context, path, text string) (string, bool) {
	path = filepath.Clean(path)
	return Excluded(withTexts(c, map[string]string{path: text}), path)
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

// FileConstraint is the build constraint the name and the header of the file at path (with text src) put
// on it, as one normalized "//go:build" line: the GOOS and GOARCH of the name's suffix, and the //go:build
// (or legacy // +build) line before the package clause, both must hold. "" when neither constrains the file.
func FileConstraint(path, src string) string {
	var parts []string
	if h := headerLine(src); h != "" {
		e, err := constraint.Parse(h)
		if err != nil {
			return h // unreadable: it matches no other file's constraint
		}
		parts = append(parts, "("+e.String()+")")
	}
	goos, goarch := suffixParts(filepath.Base(path))
	if goos != "" {
		parts = append(parts, goos)
	}
	if goarch != "" {
		parts = append(parts, goarch)
	}
	if len(parts) == 0 {
		return ""
	}
	e, err := constraint.Parse("//go:build " + strings.Join(parts, " && "))
	if err != nil {
		return ""
	}
	return "//go:build " + e.String()
}

// Equivalent reports whether the two constraints (as FileConstraint returns them; "" is no constraint) admit
// the same builds: every assignment of their tags gives both the same answer. A constraint too wide to
// enumerate is compared as text.
func Equivalent(a, b string) bool {
	ea, errA := parseOrNil(a)
	eb, errB := parseOrNil(b)
	if errA != nil || errB != nil {
		return a == b
	}
	tags := map[string]bool{}
	for _, e := range []constraint.Expr{ea, eb} {
		collectTags(e, tags)
	}
	names := make([]string, 0, len(tags))
	for t := range tags {
		names = append(names, t)
	}
	sort.Strings(names)
	if len(names) > 16 {
		return a == b
	}
	for bits := 0; bits < 1<<len(names); bits++ {
		on := map[string]bool{}
		for i, t := range names {
			on[t] = bits&(1<<i) != 0
		}
		if evalOrTrue(ea, on) != evalOrTrue(eb, on) {
			return false
		}
	}
	return true
}

// Exclusive reports whether no build satisfies both constraints a and b (as FileConstraint returns them; "" is no
// constraint): two files with exclusive constraints are never built together, so they cannot clash. A constraint
// that does not parse, or is too wide to enumerate, is not exclusive.
func Exclusive(a, b string) bool {
	ea, errA := parseOrNil(a)
	eb, errB := parseOrNil(b)
	if errA != nil || errB != nil {
		return false
	}
	tags := map[string]bool{}
	collectTags(ea, tags)
	collectTags(eb, tags)
	names := make([]string, 0, len(tags))
	for t := range tags {
		names = append(names, t)
	}
	sort.Strings(names)
	if len(names) > 16 {
		return false
	}
	for bits := 0; bits < 1<<len(names); bits++ {
		on := map[string]bool{}
		for i, t := range names {
			on[t] = bits&(1<<i) != 0
		}
		if realBuild(on, names) && evalOrTrue(ea, on) && evalOrTrue(eb, on) {
			return false
		}
	}
	return true
}

// impliedOS is the tag that a build of each GOOS sets besides its own, as suffixMatches has it: an android build
// also builds the _linux files, an ios build the _darwin files, an illumos build the _solaris files.
var impliedOS = map[string]string{"android": "linux", "ios": "darwin", "illumos": "solaris"}

// realBuild reports whether the tags on that are among names can all be set in one build: a build has one GOOS,
// with the tag it implies (see impliedOS), and one GOARCH.
func realBuild(on map[string]bool, names []string) bool {
	var oses []string
	arches := 0
	for _, t := range names {
		if !on[t] {
			continue
		}
		if knownOS[t] {
			oses = append(oses, t)
		}
		if knownArch[t] {
			arches++
		}
	}
	if arches > 1 {
		return false
	}
	switch len(oses) {
	case 0, 1:
		return true
	case 2:
		a, b := oses[0], oses[1]
		return impliedOS[a] == b || impliedOS[b] == a
	}
	return false
}

func parseOrNil(s string) (constraint.Expr, error) {
	if s == "" {
		return nil, nil
	}
	return constraint.Parse(s)
}

// evalOrTrue evaluates e under the tags set in on; a nil expression (no constraint) is true.
func evalOrTrue(e constraint.Expr, on map[string]bool) bool {
	if e == nil {
		return true
	}
	return e.Eval(func(tag string) bool { return on[tag] })
}

func collectTags(e constraint.Expr, into map[string]bool) {
	switch x := e.(type) {
	case nil:
	case *constraint.TagExpr:
		into[x.Tag] = true
	case *constraint.NotExpr:
		collectTags(x.X, into)
	case *constraint.AndExpr:
		collectTags(x.X, into)
		collectTags(x.Y, into)
	case *constraint.OrExpr:
		collectTags(x.X, into)
		collectTags(x.Y, into)
	}
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

// headerLine is the //go:build (or legacy // +build) line before the package clause, "" if none. A //go:build
// line wins. Without one, every // +build line must hold (the go tool ANDs them), so several are combined into
// one //go:build line; if one of them does not parse, the first is returned as it is.
func headerLine(src string) string {
	var plus []string
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
		case constraint.IsPlusBuild(t):
			plus = append(plus, t)
		}
	}
	switch len(plus) {
	case 0:
		return ""
	case 1:
		return plus[0]
	}
	parts := make([]string, 0, len(plus))
	for _, p := range plus {
		e, err := constraint.Parse(p)
		if err != nil {
			return plus[0]
		}
		parts = append(parts, "("+e.String()+")")
	}
	return "//go:build " + strings.Join(parts, " && ")
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

// Target is the names to look for in one declaring package: an entry of MentionsAll.
type Target struct {
	DeclFile   string   // a file of the declaring package (absolute path)
	ImportPath string   // the declaring package's import path; "" = do not search other packages
	Names      []string // the identifiers; "" and "_" are skipped
	Member     bool     // the names are methods or fields (Query.Member)
}

// Result is what MentionsAll found, and what it read to find it.
type Result struct {
	Hits  []Hit
	Walks int // walks of the tree below the root: at most one per MentionsAll call
	Files int // .go files read, in the declaring directories and below the root
}

// Kind is what a Hit mentions.
type Kind int

const (
	Use    Kind = iota // a reference to the package-level name
	Decl               // a top-level declaration of the name
	Member             // a selector or member declaration of the name (Query.Member)
	Asm                // a TEXT ·name(SB) line in a .s file of the declaring directory
)

// Hit is one mention of a name in a file the build leaves out (see the package comment).
type Hit struct {
	Path   string
	Name   string // the identifier mentioned
	Line   int    // 1-based
	Col    int    // 1-based, in bytes
	Kind   Kind   // what is mentioned
	Reason string // why the file is left out of the build; "" for an Asm hit in a built file
}

// Mentions lists the mentions of q.Name that the build of c leaves out (see the package comment).
// The declaring directory is read first; then, for exported names with an ImportPath, the files below Root
// that import it.
func Mentions(c *build.Context, q Query) []Hit {
	res, _ := MentionsAll(context.Background(), c, q.Root, q.Texts, []Target{{DeclFile: q.DeclFile, ImportPath: q.ImportPath, Names: []string{q.Name}, Member: q.Member}})
	return res.Hits
}

// MentionsAll is Mentions for many declaring files and names at once. Each declaring directory is read once,
// and the tree below root is walked at most once, for all the exported names that have an ImportPath: a file
// of it is read only when it mentions one of those names and imports their package. texts are contents that
// replace files on disk (Query.Texts). MentionsAll stops when ctx is done, returning ctx's error with the hits
// found so far.
func MentionsAll(ctx context.Context, c *build.Context, root string, texts map[string]string, targets []Target) (Result, error) {
	cc := withTexts(c, texts)
	var res Result
	var all []*scanTarget
	byDir := map[string][]*scanTarget{}
	var dirs []string
	for _, t := range targets {
		names := nonBlank(t.Names)
		if t.DeclFile == "" || len(names) == 0 {
			continue
		}
		st := newScanTarget(cc, t, names)
		all = append(all, st)
		if _, ok := byDir[st.dir]; !ok {
			dirs = append(dirs, st.dir)
		}
		byDir[st.dir] = append(byDir[st.dir], st)
	}
	for _, dir := range dirs {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			if err := ctx.Err(); err != nil {
				return res, err
			}
			p := filepath.Join(dir, e.Name())
			switch filepath.Ext(p) {
			case ".go":
				src, err := readFile(cc, p)
				if err != nil {
					continue
				}
				res.Files++
				for _, st := range byDir[dir] {
					res.Hits = append(res.Hits, scanGo(cc, st, p, src, true)...)
				}
			case ".s":
				for _, st := range byDir[dir] {
					if !st.t.Member {
						res.Hits = append(res.Hits, scanAsm(cc, st, p)...)
					}
				}
			}
		}
	}
	var cross []*scanTarget
	for _, st := range all {
		if st.t.ImportPath != "" && root != "" && len(st.exported) > 0 {
			cross = append(cross, st)
		}
	}
	if len(cross) == 0 {
		return res, nil
	}
	res.Walks++
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && (exclude.IsDirName(d.Name()) || d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		var src string
		read := false
		for _, st := range cross {
			if filepath.Dir(p) == st.dir {
				continue // the declaring directory was scanned above
			}
			if !read {
				s, err := readFile(cc, p)
				if err != nil {
					return nil
				}
				src, read = s, true
				res.Files++
			}
			res.Hits = append(res.Hits, scanGo(cc, st, p, src, false)...)
		}
		return nil
	})
	return res, err
}

// scanTarget is a Target prepared for scanning.
type scanTarget struct {
	t        Target
	dir      string          // the declaring directory
	declPkg  string          // the package clause name of DeclFile
	names    map[string]bool // the names
	exported map[string]bool // the exported names
}

func newScanTarget(c *build.Context, t Target, names []string) *scanTarget {
	st := &scanTarget{t: t, dir: filepath.Dir(t.DeclFile), declPkg: packageName(c, t.DeclFile), names: setOf(names...), exported: map[string]bool{}}
	for _, n := range names {
		if token.IsExported(n) {
			st.exported[n] = true
		}
	}
	return st
}

// nonBlank is names without "" and "_".
func nonBlank(names []string) []string {
	var out []string
	for _, n := range names {
		if n != "" && n != "_" {
			out = append(out, n)
		}
	}
	return out
}

// mentionsAny reports whether src contains one of names: a file that contains none of them cannot mention them,
// so it is not parsed.
func mentionsAny(src string, names map[string]bool) bool {
	for n := range names {
		if strings.Contains(src, n) {
			return true
		}
	}
	return false
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

// scanGo reports the mentions in one excluded .go file, whose text is src. A file of the declaring directory in
// the declaring package is read as that package; any other file, through its import of the target's ImportPath.
func scanGo(c *build.Context, st *scanTarget, path, src string, sameDir bool) []Hit {
	names := st.names
	if !sameDir {
		if st.t.ImportPath == "" || !strings.Contains(src, st.t.ImportPath) {
			return nil // a file that does not import the package is not parsed
		}
		names = st.exported
	}
	if !mentionsAny(src, names) {
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
		return Hit{Path: path, Name: id.Name, Line: p.Line, Col: p.Column, Kind: k, Reason: reason}
	}
	if sameDir && (st.declPkg == "" || f.Name.Name == st.declPkg) {
		return packageHits(f, st.names, st.t.Member, true, mk)
	}
	if st.t.ImportPath == "" || len(st.exported) == 0 {
		return nil
	}
	local, ok := importName(f, st.t.ImportPath, st.declPkg)
	if !ok {
		return nil
	}
	if local == "." {
		return packageHits(f, st.exported, st.t.Member, false, mk)
	}
	return qualifiedHits(f, st.exported, local, st.t.Member, mk)
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

// packageHits finds the mentions of the names in a file of their own package. decls says whether the top-level
// declarations of the names count (they do for the package itself, not for a dot import).
func packageHits(f *ast.File, names map[string]bool, member, decls bool, mk func(*ast.Ident, Kind) Hit) []Hit {
	role := map[*ast.Ident]int{}
	mark := func(id *ast.Ident, r int) {
		if id != nil && names[id.Name] {
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
		if !ok || !names[id.Name] {
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
			if id.Obj == nil || f.Scope != nil && f.Scope.Lookup(id.Name) == id.Obj {
				out = append(out, mk(id, Use))
			}
		case r == keyLit:
			out = append(out, mk(id, Use))
		}
		return true
	})
	return out
}

// qualifiedHits finds local.name for the names (the declaring package imported under the name local) and, for
// members, every selector of the names.
func qualifiedHits(f *ast.File, names map[string]bool, local string, member bool, mk func(*ast.Ident, Kind) Hit) []Hit {
	var out []Hit
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !names[sel.Sel.Name] {
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

// scanAsm reports the TEXT ·name(SB) lines of one .s file of the declaring directory.
func scanAsm(c *build.Context, st *scanTarget, path string) []Hit {
	src, err := readFile(c, path)
	if err != nil || !mentionsAny(src, st.names) {
		return nil
	}
	reason, _ := Excluded(c, path) // "" when the file is built
	var out []Hit
	for _, m := range asmTextRe.FindAllStringSubmatchIndex(src, -1) {
		name := src[m[2]:m[3]]
		if !st.names[name] {
			continue
		}
		line := strings.Count(src[:m[2]], "\n") + 1
		col := m[2] - strings.LastIndex(src[:m[2]], "\n")
		out = append(out, Hit{Path: path, Name: name, Line: line, Col: col, Kind: Asm, Reason: reason})
	}
	return out
}
