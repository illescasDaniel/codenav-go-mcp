package gosrc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/tools/go/packages"
)

// This file edits the import declarations as text: the lines of a spec go with its own comments, the
// untouched imports keep their order, and nothing outside the import block is printed again. A spec
// that shares its line with other code falls back to the printer-based path in decls.go.

type lineIndex struct {
	src   string
	lines []string // with their terminators
	start []int    // byte offset of each line
}

func newLineIndex(src string) *lineIndex {
	li := &lineIndex{src: src, lines: strings.SplitAfter(src, "\n")}
	if n := len(li.lines); n > 0 && li.lines[n-1] == "" {
		li.lines = li.lines[:n-1]
	}
	off := 0
	for _, l := range li.lines {
		li.start = append(li.start, off)
		off += len(l)
	}
	return li
}

// lineOf is the 0-based line holding byte offset off.
func (li *lineIndex) lineOf(off int) int {
	return sort.Search(len(li.start), func(i int) bool { return li.start[i] > off }) - 1
}

func blank(l string) bool { return strings.TrimSpace(l) == "" }

// specRange is the first and last line of a spec including its doc and trailing comments; ok is false
// when other code shares one of those lines.
func specLines(fset *token.FileSet, li *lineIndex, s *ast.ImportSpec) (first, last int, ok bool) {
	lo, hi := s.Pos(), s.End()
	if s.Doc != nil {
		lo = s.Doc.Pos()
	}
	if s.Comment != nil {
		hi = s.Comment.End()
	}
	a, b := fset.Position(lo).Offset, fset.Position(hi).Offset
	first, last = li.lineOf(a), li.lineOf(b-1)
	if !blank(li.src[li.start[first]:a]) {
		return 0, 0, false
	}
	end := len(li.src)
	if last+1 < len(li.start) {
		end = li.start[last+1]
	}
	return first, last, blank(li.src[b:end])
}

func importDecls(f *ast.File) []*ast.GenDecl {
	var out []*ast.GenDecl
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.IMPORT {
			out = append(out, g)
		}
	}
	return out
}

// removeSpec deletes one import spec with its comments. ok is false when the text-level edit does not
// apply (the caller then uses the printer).
func removeSpec(filename, src string, pick func(*ast.ImportSpec) bool) (out string, removed, ok bool) {
	fset, f, err := parse(filename, src)
	if err != nil {
		return src, false, false
	}
	li := newLineIndex(src)
	for _, g := range importDecls(f) {
		for _, s := range g.Specs {
			is := s.(*ast.ImportSpec)
			if !pick(is) {
				continue
			}
			var first, last int
			whole := !g.Lparen.IsValid() || len(g.Specs) == 1
			if whole {
				lo, hi := g.Pos(), g.End()
				if g.Doc != nil {
					lo = g.Doc.Pos()
				}
				if is.Comment != nil && is.Comment.End() > hi {
					hi = is.Comment.End()
				}
				a, b := fset.Position(lo).Offset, fset.Position(hi).Offset
				first, last = li.lineOf(a), li.lineOf(b-1)
				end := len(src)
				if last+1 < len(li.start) {
					end = li.start[last+1]
				}
				if !blank(src[li.start[first]:a]) || !blank(src[b:end]) {
					return src, false, false
				}
			} else {
				var sok bool
				if first, last, sok = specLines(fset, li, is); !sok {
					return src, false, false
				}
			}
			lines := li.lines
			from, to := first, last+1 // [from, to) is deleted
			if whole {
				// a block's own comment lines inside the parentheses go with it
				if g.Lparen.IsValid() {
					from = li.lineOf(fset.Position(g.Pos()).Offset)
					if g.Doc != nil {
						from = li.lineOf(fset.Position(g.Doc.Pos()).Offset)
					}
					to = li.lineOf(fset.Position(g.End()).Offset-1) + 1
				}
			} else {
				// A comment above the first spec of a group that goes on is that group's heading: it stays.
				if is.Doc != nil {
					startsGroup := first == 0 || blank(lines[first-1]) || strings.HasSuffix(strings.TrimSpace(lines[first-1]), "(")
					goesOn := to < len(lines) && !blank(lines[to]) && !strings.HasPrefix(strings.TrimSpace(lines[to]), ")")
					if startsGroup && goesOn {
						from = li.lineOf(fset.Position(is.Pos()).Offset)
					}
				}
			}
			prevBlank := from > 0 && blank(lines[from-1])
			nextBlank := to < len(lines) && blank(lines[to])
			nextClose := to < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[to]), ")")
			prevOpen := from > 0 && strings.HasSuffix(strings.TrimSpace(lines[from-1]), "(")
			switch {
			case prevBlank && (nextBlank || nextClose):
				from--
			case prevOpen && nextBlank:
				to++
			case whole && !prevBlank && !nextBlank && to < len(lines) && from > 0 && li.lineOf(fset.Position(f.Name.End()).Offset) == from-1:
				// the package clause now touches the next declaration
				out = strings.Join(lines[:from], "") + eolOf(lines[from-1]) + strings.Join(lines[to:], "")
				return out, true, true
			}
			out = strings.Join(lines[:from], "") + strings.Join(lines[to:], "")
			if !whole {
				out = collapseOne(filename, out)
			}
			return out, true, true
		}
	}
	return src, false, true
}

func eolOf(l string) string {
	if strings.HasSuffix(l, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

// collapseOne turns a parenthesised import declaration left with a single, comment-free spec into
// `import "x"`.
func collapseOne(filename, src string) string {
	fset, f, err := parse(filename, src)
	if err != nil {
		return src
	}
	for _, g := range importDecls(f) {
		if !g.Lparen.IsValid() || len(g.Specs) != 1 {
			continue
		}
		s := g.Specs[0].(*ast.ImportSpec)
		if s.Doc != nil || s.Comment != nil || g.Doc != nil {
			continue
		}
		inside := false
		for _, c := range f.Comments {
			if c.Pos() > g.Lparen && c.End() < g.Rparen {
				inside = true
			}
		}
		if inside {
			continue
		}
		a, b := fset.Position(g.Pos()).Offset, fset.Position(g.End()).Offset
		sa, sb := fset.Position(s.Pos()).Offset, fset.Position(s.End()).Offset
		return src[:a] + "import " + src[sa:sb] + src[b:]
	}
	return src
}

func isStd(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// addSpec inserts `alias "path"` into the first parenthesised import block, in the group of imports
// that look alike (standard library or not). Inside the group it goes to its sorted position when the
// group is sorted, else to the end. ok is false when the text-level edit does not apply.
func addSpec(filename, src, alias, path string) (out string, ok bool) {
	fset, f, err := parse(filename, src)
	if err != nil {
		return src, false
	}
	var g *ast.GenDecl
	for _, d := range importDecls(f) {
		if d.Lparen.IsValid() && len(d.Specs) > 0 {
			if p0, _ := strconv.Unquote(d.Specs[0].(*ast.ImportSpec).Path.Value); p0 == "C" && len(d.Specs) == 1 {
				continue
			}
			g = d
			break
		}
	}
	if g == nil {
		return src, false
	}
	li := newLineIndex(src)
	type grp struct {
		specs       []*ast.ImportSpec
		first, last int // lines
	}
	var groups []*grp
	for _, s := range g.Specs {
		is := s.(*ast.ImportSpec)
		first, last, sok := specLines(fset, li, is)
		if !sok {
			return src, false
		}
		if n := len(groups); n > 0 && first-groups[n-1].last <= 1 {
			groups[n-1].specs = append(groups[n-1].specs, is)
			groups[n-1].last = last
			continue
		}
		groups = append(groups, &grp{specs: []*ast.ImportSpec{is}, first: first, last: last})
	}
	ipath := func(s *ast.ImportSpec) string { p, _ := strconv.Unquote(s.Path.Value); return p }
	iname := func(s *ast.ImportSpec) string {
		if s.Name != nil {
			return s.Name.Name
		}
		return ""
	}
	var best *grp
	bestLen := -1
	for _, gr := range groups {
		for _, s := range gr.specs {
			if p := ipath(s); isStd(p) == isStd(path) {
				if n := commonPrefix(p, path); n > bestLen {
					best, bestLen = gr, n
				}
			}
		}
	}
	text := strconv.Quote(path)
	if alias != "" {
		text = alias + " " + text
	}
	eol := eolOf(li.lines[li.lineOf(fset.Position(g.Specs[0].Pos()).Offset)])
	indent := func(line int) string {
		l := li.lines[line]
		return l[:len(l)-len(strings.TrimLeft(l, " \t"))]
	}
	var at int
	var ins string
	switch {
	case best == nil && isStd(path):
		gr := groups[0]
		in := indent(gr.first)
		at, ins = li.start[gr.first], in+text+eol+eol
		if gr.first > 0 && li.lineOf(fset.Position(g.Specs[0].Pos()).Offset) != gr.first && g.Specs[0].(*ast.ImportSpec).Doc != nil {
			at = li.start[gr.first]
		}
	case best == nil:
		gr := groups[len(groups)-1]
		in := indent(gr.first)
		at, ins = li.start[gr.last]+len(li.lines[gr.last]), eol+in+text+eol
		if !strings.HasSuffix(li.lines[gr.last], "\n") {
			return src, false
		}
	default:
		sorted := true
		for i := 1; i < len(best.specs); i++ {
			a, b := best.specs[i-1], best.specs[i]
			if ipath(a) > ipath(b) || (ipath(a) == ipath(b) && iname(a) > iname(b)) {
				sorted = false
			}
		}
		in := indent(best.first)
		pos := -1
		if sorted {
			for i, s := range best.specs {
				if p := ipath(s); p > path || (p == path && iname(s) > alias) {
					pos = i
					break
				}
			}
		}
		if pos < 0 {
			if !strings.HasSuffix(li.lines[best.last], "\n") {
				return src, false
			}
			at, ins = li.start[best.last]+len(li.lines[best.last]), in+text+eol
		} else {
			first, _, _ := specLines(fset, li, best.specs[pos])
			at, ins = li.start[first], in+text+eol
		}
	}
	return src[:at] + ins + src[at:], true
}

// dotNames caches the exported names of the packages that are dot-imported.
var dotNames = struct {
	sync.Mutex
	m map[string]map[string]bool
}{m: map[string]map[string]bool{}}

// exportedNames lists the exported package-level names of an import path (nil when it cannot be loaded).
func exportedNames(dir, path string) map[string]bool {
	key := dir + "\x00" + path
	dotNames.Lock()
	defer dotNames.Unlock()
	if m, ok := dotNames.m[key]; ok {
		return m
	}
	var names map[string]bool
	cfg := &packages.Config{Dir: dir, Mode: packages.NeedName | packages.NeedTypes}
	if pkgs, err := packages.Load(cfg, path); err == nil && len(pkgs) == 1 && pkgs[0].Types != nil && len(pkgs[0].Errors) == 0 {
		names = map[string]bool{}
		for _, n := range pkgs[0].Types.Scope().Names() {
			if ast.IsExported(n) {
				names[n] = true
			}
		}
	}
	dotNames.m[key] = names
	return names
}

// usesName: some selector in f has the (package) identifier name as its qualifier.
func usesName(f *ast.File, name string) bool {
	used := false
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == name && id.Obj == nil {
				used = true
			}
		}
		return !used
	})
	return used
}

// usesDotNames: f refers, unqualified, to one of names.
func usesDotNames(f *ast.File, names map[string]bool) bool {
	sels := map[*ast.Ident]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			sels[sel.Sel] = true
		}
		return true
	})
	used := false
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Obj == nil && !sels[id] && names[id.Name] {
			used = true
		}
		return !used
	})
	return used
}

// specUsed tells whether the file uses the import spec through its own local name; sure is false when
// that cannot be told (a dot import whose package cannot be loaded).
func specUsed(dir string, f *ast.File, s *ast.ImportSpec) (used, sure bool) {
	path, _ := strconv.Unquote(s.Path.Value)
	switch {
	case s.Name != nil && s.Name.Name == "_":
		return true, true
	case s.Name != nil && s.Name.Name == ".":
		names := exportedNames(dir, path)
		if names == nil {
			return true, false
		}
		return usesDotNames(f, names), true
	case s.Name != nil:
		return usesName(f, s.Name.Name), true
	}
	return usesName(f, ImportName(path)), true
}

// unusedSpecs lists the specs of f (parsed from src) that oldF used and f no longer does.
func unusedSpecs(filename string, f, oldF *ast.File) map[string]bool {
	dir := filepath.Dir(filename)
	oldBy := map[string]*ast.ImportSpec{}
	key := func(s *ast.ImportSpec) string {
		n := ""
		if s.Name != nil {
			n = s.Name.Name
		}
		return n + " " + s.Path.Value
	}
	for _, s := range oldF.Imports {
		oldBy[key(s)] = s
	}
	out := map[string]bool{}
	for _, s := range f.Imports {
		if s.Name != nil && s.Name.Name == "_" {
			continue
		}
		if s.Path.Value == `"C"` {
			continue // import "C" carries the cgo preamble above it; it is never "unused"
		}
		os_, ok := oldBy[key(s)]
		if !ok {
			continue
		}
		if wasUsed, sure := specUsed(dir, oldF, os_); !wasUsed || !sure {
			continue
		}
		if nowUsed, sure := specUsed(dir, f, s); !nowUsed && sure {
			out[key(s)] = true
		}
	}
	return out
}

func specKey(s *ast.ImportSpec) string {
	n := ""
	if s.Name != nil {
		n = s.Name.Name
	}
	return n + " " + s.Path.Value
}

// hasSpec: src already imports path under alias.
func hasSpec(f *ast.File, alias, path string) bool {
	for _, s := range f.Imports {
		p, _ := strconv.Unquote(s.Path.Value)
		n := ""
		if s.Name != nil {
			n = s.Name.Name
		}
		if p == path && n == alias {
			return true
		}
	}
	return false
}

// AdaptImports prepares moving snippet (code of srcText) into destText: an import the destination
// already has under another local name is reused, and the snippet's qualifiers follow that name. It
// returns the snippet and the import specs it needs (`alias "path"`).
func AdaptImports(destFile, destText, srcFile, srcText, snippet string) (string, []string) {
	used := ImportsUsed(srcFile, srcText, snippet)
	if destText == "" {
		return snippet, used
	}
	_, df, err := parse(destFile, destText)
	if err != nil {
		return snippet, used
	}
	type rename struct{ from, to string }
	var renames []rename
	for i, spec := range used {
		alias, path, err := ParseImportSpec(spec)
		if err != nil || hasSpec(df, alias, path) {
			continue
		}
		for _, s := range df.Imports {
			p, _ := strconv.Unquote(s.Path.Value)
			if p != path || (s.Name != nil && (s.Name.Name == "_" || s.Name.Name == ".")) {
				continue
			}
			to := ImportName(path)
			if s.Name != nil {
				to = s.Name.Name
			}
			from := alias
			if from == "" {
				from = ImportName(path)
			}
			if from != to {
				renames = append(renames, rename{from, to})
			}
			used[i] = s.Path.Value
			if s.Name != nil {
				used[i] = s.Name.Name + " " + s.Path.Value
			}
			break
		}
	}
	if len(renames) == 0 {
		return snippet, used
	}
	const head = "package p\n"
	fset := token.NewFileSet()
	sf, err := parser.ParseFile(fset, "snippet.go", head+snippet, parser.ParseComments)
	if err != nil {
		return snippet, used
	}
	to := map[string]string{}
	for _, r := range renames {
		to[r.from] = r.to
	}
	type edit struct {
		a, b int
		s    string
	}
	var es []edit
	ast.Inspect(sf, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Obj == nil {
				if r, ok := to[id.Name]; ok {
					a := fset.Position(id.Pos()).Offset - len(head)
					es = append(es, edit{a, a + len(id.Name), r})
				}
			}
		}
		return true
	})
	sort.Slice(es, func(i, j int) bool { return es[i].a < es[j].a })
	var b strings.Builder
	pos := 0
	for _, e := range es {
		b.WriteString(snippet[pos:e.a])
		b.WriteString(e.s)
		pos = e.b
	}
	b.WriteString(snippet[pos:])
	return b.String(), used
}
