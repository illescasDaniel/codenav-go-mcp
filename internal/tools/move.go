package tools

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

type moveIn struct {
	Name              string `json:"name,omitempty" jsonschema:"top-level func, type, const or var, e.g. Scale or shapes.Square; omit to move a package directory"`
	FilePath          string `json:"file_path,omitempty" jsonschema:"narrows name; with no name: the package directory to move"`
	ToFile            string `json:"to_file" jsonschema:"destination .go file (created if missing); another directory means another package. With no name: the destination directory of a package move"`
	Apply             *bool  `json:"apply,omitempty"`
	MaxNewErrors      *int   `json:"max_new_errors,omitempty" jsonschema:"default 0; -1 = no limit; other negative values are refused"`
	IncludeDependents *bool  `json:"include_dependents,omitempty"`
	AllowLarge        bool   `json:"allow_large,omitempty"`
}

// span is a moved declaration (whole lines) in a file.
type span struct {
	file       string
	start, end int
	snippet    int // order of the snippet in the destination
}

type tedit struct {
	start, end int
	text       string
}

func applyEdits(text string, es []tedit) string {
	sort.SliceStable(es, func(i, j int) bool { return es[i].start < es[j].start })
	var b []byte
	pos := 0
	for _, e := range es {
		if e.start < pos {
			continue
		}
		b = append(b, text[pos:e.start]...)
		b = append(b, e.text...)
		pos = e.end
		if e.text == "" && e.start < e.end && atLineStart(text, e.start) && atLineStart(text, e.end) {
			b, pos = tidySeam(b, text, pos)
		}
	}
	b = append(b, text[pos:]...)
	return string(b)
}

func atLineStart(text string, off int) bool {
	return off == 0 || off >= len(text) || text[off-1] == '\n'
}

// blankLen is the length of the run of blank lines at the start of s.
func blankLen(s string) int {
	n := 0
	for n < len(s) {
		i := strings.IndexByte(s[n:], '\n')
		if i < 0 || strings.TrimSpace(s[n:n+i]) != "" {
			break
		}
		n += i + 1
	}
	return n
}

// tidySeam runs after whole lines were cut out between b and text[pos:]: the blank lines on both sides
// of the cut collapse to one, and a cut at the end of the file leaves no blank line behind.
func tidySeam(b []byte, text string, pos int) ([]byte, int) {
	post := text[pos:]
	pre := len(b)
	for pre > 0 {
		i := strings.LastIndexByte(string(b[:pre-1]), '\n') + 1
		if pre-1 < i || strings.TrimSpace(string(b[i:pre])) != "" {
			break
		}
		pre = i
	}
	prevBlank := pre < len(b) // b ends with blank lines
	switch {
	case strings.TrimSpace(post) == "":
		if prevBlank && pre > 0 {
			b = b[:pre]
		}
	case prevBlank:
		pos += blankLen(post)
		if nl := strings.IndexByte(string(b[pre:]), '\n'); nl >= 0 && pre+nl+1 < len(b) {
			b = b[:pre+nl+1]
		}
	}
	return b, pos
}
func moveSymbol(ctx context.Context, w *Workspace, in moveIn) (string, error) {
	if in.Name == "" && in.FilePath != "" && in.ToFile != "" {
		return moveDirectory(ctx, w, in.FilePath, in.ToFile, in)
	}
	if in.Name == "" || in.ToFile == "" {
		return "", toolerr.Inputf("move needs name + to_file (a symbol), or file_path + to_file as directories (a package).")
	}
	l, err := locate(ctx, w, in.Name, in.FilePath)
	if err != nil {
		return "", err
	}
	d := l.decl
	dest, err := w.absPath(in.ToFile)
	if err != nil {
		return "", err
	}
	if filepath.Ext(dest) != ".go" {
		return "", toolerr.Inputf("%q is not a .go file.", in.ToFile)
	}
	if dest == l.path {
		return "", toolerr.Inputf("%s is already in %s.", in.Name, in.ToFile)
	}
	if d.Method && filepath.Dir(dest) != filepath.Dir(l.path) {
		return "", toolerr.Inputf("%s is a method of %s; Go methods must stay in their type's package. Move %s (its methods follow) or pick a file in the same directory.", in.Name, d.Recv, d.Recv)
	}
	if sp := specialFunc(d, gosrc.PackageName(l.text)); sp != "" && filepath.Dir(dest) != filepath.Dir(l.path) {
		return "", toolerr.Inputf("%s is the %s function and must stay in its package; pick a file in the same directory.", in.Name, sp)
	}
	srcT, dstT := strings.HasSuffix(l.path, "_test.go"), strings.HasSuffix(dest, "_test.go")
	if srcT && !dstT && d.Keyword == "func" && !d.Method && isTestFunc(d.Name) {
		return "", toolerr.Inputf("%s is a test function (Test/Benchmark/Fuzz/Example) and go test only finds those in _test.go files; pick a _test.go destination.", in.Name)
	}
	if !srcT && dstT {
		locs, err := w.Client.References(ctx, l.path, l.sym.Line+1, l.sym.Column+1, false)
		if err != nil {
			return "", err
		}
		var prod []string
		for _, r := range locs {
			p := lsp.URIToPath(r.URI)
			if strings.HasSuffix(p, "_test.go") {
				continue
			}
			if p == l.path {
				if off := edits.Offset(l.text, r.Range.Start.Line, r.Range.Start.Character); off >= d.Start && off < d.End {
					continue // the declaration referring to itself
				}
			}
			prod = append(prod, fmt.Sprintf("%s:%d", edits.RelativeName(p, w.Root), r.Range.Start.Line+1))
		}
		if len(prod) > 0 {
			return "", toolerr.Inputf("%s is used by production code: %s; code in a _test.go file is not part of the normal build.", in.Name, strings.Join(prod[:min(len(prod), 5)], ", "))
		}
	}
	if gosrc.UsesCgo(l.path, l.text, d.Start, d.End) {
		return "", toolerr.Inputf("%s uses cgo (C.xxx); the import \"C\" preamble must stay with it, so it can't be moved automatically.", in.Name)
	}
	var notes []string
	if d.Grouped || d.MultiName {
		if l, err = carveOut(l); err != nil {
			return "", err
		}
		d = l.decl
		if d.Keyword == "var" {
			notes = append(notes, "Moving a var can change the order of package initialisation side effects.")
		}
	}
	var plan *edits.EditPlan
	if filepath.Dir(dest) == filepath.Dir(l.path) {
		plan, err = moveWithinPackage(l, dest)
	} else {
		plan, err = moveAcrossPackages(ctx, w, l, dest, in.Name)
	}
	if err != nil {
		return "", err
	}
	plan.Notes = append(plan.Notes, notes...)
	return finishPlan(ctx, w, plan, flag(in.Apply, true), limit(in.MaxNewErrors), flag(in.IncludeDependents, true), in.AllowLarge)
}

func wholeLines(text string, start, end int) (int, int) {
	return lineStart(text, start), lineEnd(text, end)
}

func appendTo(destText, snippet string) string {
	return strings.TrimRight(destText, "\r\n") + "\n\n" + snippet + "\n"
}

func moveWithinPackage(l *located, dest string) (*edits.EditPlan, error) {
	d := l.decl
	destText, destBOM, rerr := edits.ReadSource(dest)
	exists := rerr == nil
	if rerr != nil && !os.IsNotExist(rerr) {
		return nil, rerr
	}
	snippet := l.text[d.Start:d.End]
	used := gosrc.ImportsUsed(l.path, l.text, snippet)
	if exists {
		// an import the file already has (under any local name) is reused
		snippet, used = gosrc.AdaptImports(dest, destText, l.path, l.text, snippet)
		if gosrc.PackageName(destText) != gosrc.PackageName(l.text) {
			return nil, toolerr.Inputf("%s belongs to package %s, not %s.", filepath.Base(dest), gosrc.PackageName(destText), gosrc.PackageName(l.text))
		}
		key := d.Name
		if d.Method {
			key = d.Recv + "." + d.Name
		}
		for _, n := range gosrc.FileDeclNames(dest, destText) {
			if n == key {
				return nil, toolerr.Inputf("%s is already declared in %s.", d.Name, filepath.Base(dest))
			}
		}
		destText = appendTo(destText, snippet)
	} else {
		destText = "package " + gosrc.PackageName(l.text) + "\n\n" + snippet + "\n"
	}
	destText, err := gosrc.EditImports(dest, destText, "", used, false)
	if err != nil {
		return nil, err
	}
	from, to := wholeLines(l.text, d.Start, d.End)
	srcText, err := gosrc.EditImports(l.path, applyEdits(l.text, []tedit{{from, to, ""}}), l.text, nil, true)
	if err != nil {
		return nil, err
	}
	src, err := fileChange(l.path, srcText, &l.orig, l.bom)
	if err != nil {
		return nil, err
	}
	var dc edits.FileChange
	if exists {
		old, _, _ := edits.ReadSource(dest)
		dc, err = fileChange(dest, destText, &old, destBOM)
	} else {
		dc, err = fileChange(dest, destText, nil, l.bom) // and its byte-order mark
		if err == nil {
			dc.New = edits.Str(edits.MatchEOL(*dc.New, l.orig)) // a new file takes the line endings of the file it came from
		}
	}
	if err != nil {
		return nil, err
	}
	return edits.NewPlan("move "+d.Name+" to "+filepath.Base(dest), []edits.FileChange{src, dc}), nil
}

func goDeps(root, pkg string) map[string]bool {
	out, err := exec.Command("go", "list", "-C", root, "-deps", "-f", "{{.ImportPath}}", pkg).Output()
	if err != nil {
		return nil
	}
	m := map[string]bool{}
	for _, p := range strings.Fields(string(out)) {
		m[p] = true
	}
	return m
}

func pkgLevel(o types.Object) bool { return o.Pkg() != nil && o.Parent() == o.Pkg().Scope() }

func recvBase(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvBase(t.X)
	case *ast.ParenExpr:
		return recvBase(t.X)
	case *ast.IndexExpr:
		return recvBase(t.X)
	case *ast.IndexListExpr:
		return recvBase(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

func moveAcrossPackages(ctx context.Context, w *Workspace, l *located, dest, display string) (*edits.EditPlan, error) {
	d := l.decl
	fset := token.NewFileSet()
	cfg := &packages.Config{
		Context: ctx, Dir: w.Root, Fset: fset, Tests: true,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedModule,
	}
	if fl := strings.Fields(os.Getenv("CODENAV_GO_MCP_BUILD_FLAGS")); len(fl) > 0 {
		cfg.BuildFlags = fl
	}
	if l.text != l.orig {
		cfg.Overlay = map[string][]byte{l.path: []byte(l.text)}
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, err
	}
	destDir := filepath.Dir(dest)
	var A, B *packages.Package
	find := func() {
		for _, p := range pkgs {
			if strings.HasSuffix(p.PkgPath, "_test") || strings.Contains(p.ID, " [") || strings.HasSuffix(p.ID, ".test") {
				continue
			}
			for _, f := range p.GoFiles {
				if f == l.path {
					A = p
				}
				if filepath.Dir(f) == destDir {
					B = p
				}
			}
		}
	}
	find()
	if A == nil {
		return nil, toolerr.Inputf("%s is not part of a package in this workspace's module.", filepath.Base(l.path))
	}
	// A destination in another module (a nested go.mod) is not covered by "./..." of the root module:
	// its import path comes from its own go.mod, and its packages are loaded separately.
	destMod, haveDestMod := gomod.Owner(destDir)
	crossModule := haveDestMod && A.Module != nil && destMod.Dir != A.Module.Dir
	if crossModule && B == nil {
		if rel, err := filepath.Rel(w.Root, destMod.Dir); err == nil && !strings.HasPrefix(rel, "..") {
			ncfg := *cfg
			ncfg.Dir = destMod.Dir
			if more, err := packages.Load(&ncfg, "./..."); err == nil {
				pkgs = append(pkgs, more...)
				find()
			}
		}
	}
	// Files that import "C" are type-checked from cgo's generated output, so their syntax positions do not
	// match the source: they are scanned lexically instead, and a use of the moved name in one is refused.
	cgoFile := map[string]bool{}
	for _, p := range pkgs {
		for _, f := range p.GoFiles {
			if !cgoFile[f] && importsC(f) {
				cgoFile[f] = true
			}
		}
	}
	if cgoFile[l.path] {
		return nil, toolerr.Inputf("%s is in %s, a file that imports \"C\"; its declarations can't be moved automatically.", display, filepath.Base(l.path))
	}
	if len(A.Errors) > 0 {
		return nil, toolerr.Inputf("package %s has errors (%v); fix them before moving code out of it.", A.PkgPath, A.Errors[0])
	}
	bName, bPath := "", ""
	switch {
	case B != nil:
		bName, bPath = B.Name, B.PkgPath
	case crossModule:
		bPath, err = importPathOf(moduleInfo{Path: destMod.Path, Dir: destMod.Dir}, destDir)
		if err != nil {
			return nil, err
		}
		bName = packageFor(dest)
	case A.Module != nil:
		rel, err := filepath.Rel(A.Module.Dir, destDir)
		if err != nil || strings.HasPrefix(rel, "..") {
			return nil, toolerr.Inputf("%s is outside the module %s.", filepath.Base(destDir), A.Module.Path)
		}
		bName, bPath = packageFor(dest), A.Module.Path+"/"+filepath.ToSlash(rel)
	default:
		return nil, toolerr.Inputf("cannot work out the import path of %s.", destDir)
	}
	if B != nil && B.Types != nil && B.Types.Scope().Lookup(d.Name) != nil {
		return nil, toolerr.Inputf("package %s already declares %s.", bPath, d.Name)
	}

	texts := map[string]*struct {
		text string
		bom  bool
	}{}
	read := func(f string) (string, bool, error) {
		if t, ok := texts[f]; ok {
			return t.text, t.bom, nil
		}
		s, b, err := edits.ReadSource(f)
		if err != nil {
			return "", false, err
		}
		texts[f] = &struct {
			text string
			bom  bool
		}{s, b}
		return s, b, nil
	}
	texts[l.path] = &struct {
		text string
		bom  bool
	}{l.text, l.bom}
	// Offsets in a file that starts with a BOM count its 3 bytes; the text we edit has them stripped.
	off := func(p token.Pos) int {
		pos := fset.Position(p)
		if _, b, err := read(pos.Filename); err == nil && b {
			return pos.Offset - len("\xef\xbb\xbf")
		}
		return pos.Offset
	}

	// The declaration and, for a type, its methods wherever they live in the package.
	spans := []span{{file: l.path, snippet: 0}}
	spans[0].start, spans[0].end = d.Start, d.End // the declaration's own text; deleting it widens to whole lines
	if d.Keyword == "type" {
		seen := map[string]bool{}
		for _, p := range pkgs {
			if p.PkgPath != A.PkgPath {
				continue
			}
			for _, f := range p.Syntax {
				name := fset.Position(f.Pos()).Filename
				if seen[name] {
					continue
				}
				seen[name] = true
				for _, decl := range f.Decls {
					fd, ok := decl.(*ast.FuncDecl)
					if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 || recvBase(fd.Recv.List[0].Type) != d.Name {
						continue
					}
					start := fd.Pos()
					if fd.Doc != nil {
						start = fd.Doc.Pos()
					}
					end := off(fd.End())
					for _, cg := range f.Comments { // a trailing comment on the closing line travels with the method
						if cg.Pos() >= fd.End() && fset.Position(cg.Pos()).Line == fset.Position(fd.End()).Line {
							end = off(cg.End())
							break
						}
					}
					spans = append(spans, span{file: name, start: off(start), end: end, snippet: len(spans)})
				}
			}
		}
	}
	inSpan := func(file string, o int) int {
		for i, s := range spans {
			if s.file == file && o >= s.start && o < s.end {
				return i
			}
		}
		return -1
	}
	inM := func(p token.Pos) bool {
		if !p.IsValid() {
			return false
		}
		return inSpan(fset.Position(p).Filename, off(p)) >= 0
	}

	var refusals []string
	refuse := func(format string, a ...any) {
		m := fmt.Sprintf(format, a...)
		for _, r := range refusals {
			if r == m {
				return
			}
		}
		refusals = append(refusals, m)
	}
	fileEdits := map[string][]tedit{}
	addImport := map[string][]string{}
	needA := map[int]bool{}       // spans that now refer to package A by qualifier
	usesM := false                // code left in A refers to the moved declaration
	dotMoved := map[string]bool{} // files that dot-import A and use the moved declaration
	dotOther := map[string]bool{} // files that dot-import A and still use something else from it
	done := map[string]bool{}
	where := func(file string, o int) string {
		t, _, _ := read(file)
		line, _ := edits.PositionOf(t, o)
		return fmt.Sprintf("%s:%d", edits.RelativeName(file, w.Root), line+1)
	}
	for _, p := range pkgs {
		isA := p.PkgPath == A.PkgPath
		isB := bPath != "" && p.PkgPath == bPath
		for _, f := range p.Syntax {
			fname := fset.Position(f.Pos()).Filename
			if done[fname] || p.TypesInfo == nil {
				continue
			}
			if cgoFile[fname] {
				done[fname] = true
				if usesNameLexically(fname, d.Name, A.PkgPath, isA) {
					refuse("%s refers to %s, but it imports \"C\", so its callers can't be rewritten automatically (edit that file by hand)", edits.RelativeName(fname, w.Root), d.Name)
				}
				continue
			}
			done[fname] = true
			handled := map[*ast.Ident]bool{}
			dotA := false
			for _, im := range f.Imports {
				if ip, _ := strconv.Unquote(im.Path.Value); im.Name != nil && im.Name.Name == "." && ip == A.PkgPath {
					dotA = true
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					x, ok := n.X.(*ast.Ident)
					if !ok {
						return true
					}
					pn, ok := p.TypesInfo.Uses[x].(*types.PkgName)
					if !ok {
						return true
					}
					handled[x], handled[n.Sel] = true, true
					obj := p.TypesInfo.Uses[n.Sel]
					sp := inSpan(fname, off(x.Pos()))
					path := pn.Imported().Path()
					switch {
					case sp < 0 && path == A.PkgPath && obj != nil && inM(obj.Pos()):
						if pkgLevel(obj) {
							if isB {
								fileEdits[fname] = append(fileEdits[fname], tedit{off(x.Pos()), off(n.Sel.Pos()), ""})
							} else {
								fileEdits[fname] = append(fileEdits[fname], tedit{off(x.Pos()), off(x.End()), bName})
								addImport[fname] = append(addImport[fname], bPath)
							}
						} else if !obj.Exported() {
							refuse("%s uses unexported %s", where(fname, off(n.Sel.Pos())), obj.Name())
						}
					case sp >= 0 && path == bPath:
						fileEdits[fname] = append(fileEdits[fname], tedit{off(x.Pos()), off(n.Sel.Pos()), ""})
					case sp >= 0 && obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == A.PkgPath && !inM(obj.Pos()) && !obj.Exported():
						refuse("the moved code uses unexported %s of package %s (%s)", obj.Name(), A.Name, where(fname, off(n.Sel.Pos())))
					}
				case *ast.Ident:
					if handled[n] {
						return true
					}
					obj := p.TypesInfo.Uses[n]
					if obj == nil || obj.Pkg() == nil || obj.Pkg().Path() != A.PkgPath {
						return true
					}
					o := off(n.Pos())
					if dotA && !isA && inSpan(fname, o) < 0 && pkgLevel(obj) {
						// import . "A": the name is used unqualified, so the file needs the destination instead.
						if inM(obj.Pos()) {
							dotMoved[fname] = true
							if !isB {
								addImport[fname] = append(addImport[fname], ". "+strconv.Quote(bPath))
							}
						} else {
							dotOther[fname] = true
						}
						return true
					}
					if sp := inSpan(fname, o); sp >= 0 {
						switch {
						case inM(obj.Pos()):
						case pkgLevel(obj) && !obj.Exported():
							refuse("the moved code uses unexported %s of package %s (%s); export it or move it too", obj.Name(), A.Name, where(fname, o))
						case pkgLevel(obj):
							fileEdits[fname] = append(fileEdits[fname], tedit{o, o, A.Name + "."})
							needA[sp] = true
						case obj.Parent() == nil && !obj.Exported():
							refuse("the moved code uses unexported %s of package %s (%s)", obj.Name(), A.Name, where(fname, o))
						}
					} else if inM(obj.Pos()) {
						switch {
						case pkgLevel(obj) && isA:
							usesM = true
							fileEdits[fname] = append(fileEdits[fname], tedit{o, o + len(n.Name), bName + "." + n.Name})
							addImport[fname] = append(addImport[fname], bPath)
						case obj.Parent() == nil && !obj.Exported():
							refuse("%s uses unexported %s, which moves to another package", where(fname, o), obj.Name())
						}
					}
				}
				return true
			})
		}
	}
	if bName == "main" {
		var importers []string
		for f, specs := range addImport {
			for _, sp := range specs {
				if sp == bPath || sp == ". "+strconv.Quote(bPath) {
					importers = append(importers, edits.RelativeName(f, w.Root))
					break
				}
			}
		}
		if len(importers) > 0 {
			sort.Strings(importers)
			more := ""
			if len(importers) > 3 {
				more = fmt.Sprintf(" and %d more", len(importers)-3)
				importers = importers[:3]
			}
			refuse("the destination is package main (a program); nothing can import it, but %s use(s) %s", strings.Join(importers, ", ")+more, display)
		}
	}
	anyNeedA := len(needA) > 0
	if B != nil || anyNeedA || usesM {
		depsB, depsA := map[string]bool{}, goDeps(w.Root, A.PkgPath)
		if B != nil {
			depsB = goDeps(w.Root, bPath)
		}
		if (usesM && (depsB[A.PkgPath] || anyNeedA)) || (anyNeedA && depsA[bPath]) {
			refuse("the move would create an import cycle between %s and %s", A.PkgPath, bPath)
		}
	}
	if len(refusals) > 0 {
		sort.Strings(refusals)
		return nil, toolerr.Inputf("Cannot move %s to %s:\n  - %s", display, bPath, strings.Join(refusals[:min(len(refusals), 12)], "\n  - "))
	}

	// Cut the snippets out (with their qualifier edits applied) and delete the spans.
	snippets := make([]string, len(spans))
	snippetImports := map[string]bool{}
	byFile := map[string][]int{}
	for i, s := range spans {
		byFile[s.file] = append(byFile[s.file], i)
	}
	destPre, _, _ := edits.ReadSource(dest) // "" for a new file: its imports are the snippets' own
	type result struct {
		next, old string
		bom       bool
	}
	changed := map[string]*result{}
	files := map[string]bool{}
	for f := range byFile {
		files[f] = true
	}
	for f := range fileEdits {
		files[f] = true
	}
	for f := range dotMoved {
		files[f] = true
	}
	for f := range files {
		text, bom, err := read(f)
		if err != nil {
			return nil, err
		}
		var outer []tedit
		inner := map[int][]tedit{}
		for _, e := range fileEdits[f] {
			if sp := inSpan(f, e.start); sp >= 0 {
				inner[sp] = append(inner[sp], tedit{e.start - spans[sp].start, e.end - spans[sp].start, e.text})
			} else {
				outer = append(outer, e)
			}
		}
		for _, i := range byFile[f] {
			s := spans[i]
			snippets[i] = applyEdits(text[s.start:s.end], inner[i])
			var used []string
			snippets[i], used = gosrc.AdaptImports(dest, destPre, f, text, snippets[i])
			for _, spec := range used {
				snippetImports[spec] = true
			}
			a, b := wholeLines(text, s.start, s.end)
			outer = append(outer, tedit{a, b, ""})
		}
		changed[f] = &result{next: applyEdits(text, outer), old: text, bom: bom}
	}
	order := make([]int, len(spans))
	for i := range order {
		order[i] = i
	}
	var parts []string
	for _, i := range order {
		parts = append(parts, strings.TrimSpace(snippets[i]))
	}
	code := strings.Join(parts, "\n\n")

	var out []edits.FileChange
	paths := make([]string, 0, len(changed))
	for f := range changed {
		paths = append(paths, f)
	}
	sort.Strings(paths)
	for _, f := range paths {
		r := changed[f]
		next, err := gosrc.EditImports(f, r.next, r.old, addImport[f], true)
		if err != nil {
			return nil, err
		}
		if dotMoved[f] && !dotOther[f] {
			if next, err = gosrc.DropImport(f, next, ".", A.PkgPath); err != nil {
				return nil, err
			}
		}
		old := r.old
		if f == l.path {
			old = l.orig
		}
		c, err := fileChange(f, next, &old, r.bom)
		if err != nil {
			return nil, err
		}
		if !c.IsNoop() {
			out = append(out, c)
		}
	}

	destText, destBOM, rerr := edits.ReadSource(dest)
	exists := rerr == nil
	if rerr != nil && !os.IsNotExist(rerr) {
		return nil, rerr
	}
	var specs []string
	for s := range snippetImports {
		specs = append(specs, s)
	}
	sort.Strings(specs)
	if anyNeedA {
		specs = append(specs, A.PkgPath)
	}
	var next string
	if exists {
		if n := gosrc.PackageName(destText); n != bName {
			return nil, toolerr.Inputf("%s belongs to package %s, not %s.", filepath.Base(dest), n, bName)
		}
		next = appendTo(destText, code)
	} else {
		next = "package " + bName + "\n\n" + code + "\n"
	}
	if next, err = gosrc.EditImports(dest, next, "", specs, false); err != nil {
		return nil, err
	}
	var dc edits.FileChange
	if exists {
		dc, err = fileChange(dest, next, &destText, destBOM)
	} else {
		dc, err = fileChange(dest, next, nil, l.bom)
		if err == nil {
			dc.New = edits.Str(edits.MatchEOL(*dc.New, l.orig))
		}
	}
	if err != nil {
		return nil, err
	}
	out = append(out, dc)

	notes := []string{fmt.Sprintf("Moved %s from package %s to %s (%d declaration(s), including methods).", display, A.PkgPath, bPath, len(spans))}
	if n := len(fileEdits); n > 0 {
		notes = append(notes, fmt.Sprintf("Package qualifiers were rewritten in %d file(s).", n))
	}
	if crossModule {
		notes = append(notes, fmt.Sprintf("%s moved to another module (%s -> %s): packages of %s resolve %s only through a go.work that joins both modules, or a require/replace in their go.mod.", display, A.Module.Path, destMod.Path, A.Module.Path, bPath))
	}
	return edits.NewPlan("move "+display+" to "+edits.RelativeName(dest, w.Root), out, notes...), nil
}

// importsC: the Go file at path has `import "C"`.
func importsC(path string) bool {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		return false
	}
	for _, im := range f.Imports {
		if im.Path.Value == `"C"` {
			return true
		}
	}
	return false
}

// usesNameLexically: the file at path mentions name as the moved declaration would be reached from
// it: unqualified in a file of the declaring package (inPkg), else through an import of pkgPath.
func usesNameLexically(path, name, pkgPath string, inPkg bool) bool {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return true // unreadable: be careful
	}
	quals := map[string]bool{}
	dot := false
	for _, im := range f.Imports {
		if p, _ := strconv.Unquote(im.Path.Value); p == pkgPath {
			switch {
			case im.Name == nil:
				quals[pathBase(p)] = true
			case im.Name.Name == ".":
				dot = true
			default:
				quals[im.Name.Name] = true
			}
		}
	}
	found := false
	sels := map[*ast.Ident]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			sels[sel.Sel] = true
			if x, ok := sel.X.(*ast.Ident); ok && quals[x.Name] && sel.Sel.Name == name {
				found = true
			}
		}
		return !found
	})
	if inPkg || dot {
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == name && !sels[id] {
				found = true
			}
			return !found
		})
	}
	return found
}

func pathBase(p string) string { return gosrc.ImportName(p) }
