// Package resolve turns a name ("Square", "Square.Area", "shapes.Scale") into one symbol position.
package resolve

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/illescasDaniel/codenav-go-mcp/internal/format"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
	"github.com/illescasDaniel/codenav-go-mcp/internal/index"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

// Symbol is a resolved symbol; Line/Column are 0-based (LSP).
type Symbol struct {
	Name          string
	Kind          int
	URI           string
	Line, Column  int
	ContainerName string
	// Unbuilt: only the symbol index knows the declaration, and its file carries a build constraint, so
	// gopls (current GOOS/GOARCH/tags) does not type-check it.
	Unbuilt bool
	// CaseFolded is set when the query matched only ignoring case: the spelling that really exists.
	CaseFolded string
}

// Rel is the workspace-relative path of the symbol's file.
func (s Symbol) Rel(root string) string { return format.Relative(s.URI, root) }

func fromLSP(s *lsp.Symbol) Symbol {
	uri, line, col := format.SymbolPosition(s)
	return Symbol{Name: s.Name, Kind: s.Kind, URI: uri, Line: line, Column: col, ContainerName: s.ContainerName}
}

func pkgBase(container string) string { return path.Base(container) }

// matches: 0 no, 1 case-insensitive, 2 exact. Accepts "Name", "Type.Member" and "pkg.Name"/"pkg.Type.Member".
func matches(s *lsp.Symbol, q string) int {
	best := 0
	try := func(cand string) {
		switch {
		case cand == q:
			best = 2
		case strings.EqualFold(cand, q) && best < 1:
			best = 1
		}
	}
	try(s.Name)
	if s.ContainerName != "" {
		try(pkgBase(s.ContainerName) + "." + s.Name)
		try(s.ContainerName + "." + s.Name)
	}
	return best
}

// foldedForm is the spelling of s that equals q ignoring case ("Gen" for q "gen", "gen.Box" for "gen.box"),
// "" when none does.
func foldedForm(s *lsp.Symbol, q string) string {
	cands := []string{s.Name}
	if s.ContainerName != "" {
		cands = append(cands, pkgBase(s.ContainerName)+"."+s.Name, s.ContainerName+"."+s.Name)
	}
	for _, c := range cands {
		if strings.EqualFold(c, q) {
			return c
		}
	}
	return ""
}

// fileTarget turns file_path (relative, ./relative or absolute) into a clean workspace-relative path
// when it lies inside root; ok reports whether it did.
func fileTarget(root, file string) (target string, ok bool) {
	file = strings.ReplaceAll(file, "\\", "/")
	if filepath.IsAbs(file) {
		for _, r := range [][2]string{{root, file}, {evalLink(root), evalLink(file)}} {
			if rel, err := filepath.Rel(r[0], r[1]); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
				return filepath.ToSlash(rel), true
			}
		}
		return path.Clean(file), false
	}
	return path.Clean(file), true
}

func evalLink(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// matchesFile compares a symbol's file with file_path. exact is true for a path that names the
// file from the workspace root ("main.go" is the root's main.go, not every main.go); otherwise it
// accepts a trailing part of the path ("tool/main.go").
func matchesFile(s *lsp.Symbol, root, file string, exact bool) bool {
	rel := format.Relative(s.Location.URI, root)
	target, inside := fileTarget(root, file)
	if exact {
		return inside && rel == target
	}
	return rel == target || strings.HasSuffix(rel, "/"+target) || strings.HasSuffix(target, "/"+rel)
}

// pkgLabel is the import path of the package a candidate lives in: gopls reports it, the symbol index
// only knows the package name, so that one is completed from the nearest go.mod.
// qualifiedExample is the candidate's own name with its package name in front ("shapes.Scale").
func qualifiedExample(c *lsp.Symbol, query string) string {
	if strings.Contains(query, ".") || c.ContainerName == "" {
		return ""
	}
	return path.Base(c.ContainerName) + "." + query
}

func pkgLabel(c *lsp.Symbol) string {
	if strings.Contains(c.ContainerName, "/") || c.Location == nil {
		return c.ContainerName
	}
	dir := filepath.Dir(lsp.URIToPath(c.Location.URI))
	if m, ok := gomod.Owner(dir); ok {
		if rel, err := filepath.Rel(m.Dir, dir); err == nil && !strings.HasPrefix(rel, "..") {
			if rel == "." {
				return m.Path
			}
			return m.Path + "/" + filepath.ToSlash(rel)
		}
	}
	return c.ContainerName
}

// candidateLines lists ambiguous candidates alike: package by import path and, as soon as one of them
// sits behind a build constraint, the constraint (or its absence) on every line.
func candidateLines(cands []*lsp.Symbol, root string) string {
	constrained := false
	for _, c := range cands {
		constrained = constrained || c.Detail != ""
	}
	lines := make([]string, len(cands))
	for i, c := range cands {
		plain := *c
		plain.Detail = ""
		lines[i] = "  " + format.FormatWorkspaceSymbol(&plain, root)
		if l := pkgLabel(c); l != "" {
			lines[i] += "  in " + l
		}
		switch {
		case c.Detail != "":
			lines[i] += "  (build: " + c.Detail + ")"
		case constrained:
			lines[i] += "  (no build constraint)"
		}
	}
	return strings.Join(lines, "\n")
}

// NotFoundError is a name that matches no workspace symbol. Its Err is an InputError marked NotFound
// (a valid query with no match, not a refusal); callers that know more about the language can replace
// the hint (see Query).
type NotFoundError struct {
	Query string
	Err   *toolerr.InputError
	// Suggest holds the symbols that match Query when the case is ignored (set by strict resolution).
	Suggest []string
}

func (e *NotFoundError) Error() string { return e.Err.Msg }
func (e *NotFoundError) Unwrap() error { return e.Err }

// SymbolCap is gopls's limit on workspace/symbol results; a result list this long may have
// dropped the exact match in favour of fuzzy ones.
const SymbolCap = lsp.WorkspaceSymbolCap

// lookup queries workspace/symbol for last (the final dotted part of query). When gopls's result
// list is capped, the exact symbol can be missing ("U" among 800 "U0_n"), so it also asks for
// names ending in last ("U$") and, for a dotted query, the qualified form ("short.U").
func lookup(ctx context.Context, c *lsp.Client, query, last string) ([]*lsp.Symbol, error) {
	syms, err := c.WorkspaceSymbol(ctx, last)
	if err != nil {
		return nil, err
	}
	if len(syms) >= SymbolCap {
		extra := []string{last + "$"}
		if query != last {
			extra = append(extra, query)
		}
		for _, q := range extra {
			more, err := c.WorkspaceSymbol(ctx, q)
			if err != nil {
				return nil, err
			}
			for _, s := range more {
				// A package-qualified query makes gopls report "short.U"; keep names as the plain query does.
				if pb := pkgBase(s.ContainerName) + "."; s.ContainerName != "" && strings.HasPrefix(s.Name, pb) && strings.HasPrefix(q, pb) {
					s.Name = strings.TrimPrefix(s.Name, pb)
				}
				syms = append(syms, s)
			}
		}
	}
	var all []*lsp.Symbol
	seen := map[string]bool{}
	for i := range syms {
		s := &syms[i]
		if s.Location == nil {
			continue
		}
		uri, l, col := format.SymbolPosition(s)
		key := fmt.Sprintf("%s|%s|%d|%d", s.Name, uri, l, col)
		if seen[key] {
			continue
		}
		seen[key] = true
		all = append(all, s)
	}
	return all, nil
}

// canonPath is the symlink-free path of a file URI, so one declaration reached through two spellings
// (a replace directive, a symlinked checkout, the index's walk vs gopls's view) compares equal.
func canonPath(uri string) string {
	if p, ok := canonCache.Load(uri); ok {
		return p.(string)
	}
	p := evalLink(filepath.Clean(lsp.URIToPath(uri)))
	canonCache.Store(uri, p)
	return p
}

// canonCache memoises canonPath (one syscall per file instead of one per comparison).
var canonCache sync.Map

// dedupeSymbols drops later symbols that sit at the same file, line and column as an earlier one.
func dedupeSymbols(in []*lsp.Symbol) []*lsp.Symbol {
	type key struct {
		file      string
		line, col int
	}
	seen := map[key]bool{}
	var out []*lsp.Symbol
	for _, s := range in {
		uri, l, c := format.SymbolPosition(s)
		k := key{canonPath(uri), l, c}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	return out
}

// FromEntry converts an index entry to a workspace symbol; Detail carries the build constraint.
func FromEntry(e index.Entry) *lsp.Symbol {
	end := e.Col
	for _, r := range e.Name {
		end += utf16.RuneLen(r)
	}
	return &lsp.Symbol{
		Name: e.Qual(), Kind: e.Kind, ContainerName: e.Pkg, Detail: e.Constraint,
		Location: &lsp.Location{URI: lsp.PathToURI(e.File), Range: lsp.Range{
			Start: lsp.Position{Line: e.Line, Character: e.Col},
			End:   lsp.Position{Line: e.Line, Character: end},
		}},
	}
}

// merge adds the declarations the index knows and gopls did not report to the exact and loose lists
// (matched on file and line). Where gopls has the symbol it stays the source of kind and position; a
// build constraint on its file is noted in Detail. onlyIndex marks the symbols that came from the index.
func merge(ix *index.Index, query string, exact, loose []*lsp.Symbol) (ex, lo []*lsp.Symbol, onlyIndex map[*lsp.Symbol]bool) {
	onlyIndex = map[*lsp.Symbol]bool{}
	if ix == nil {
		return exact, loose, onlyIndex
	}
	ie, il := ix.Lookup(query)
	fold := func(have []*lsp.Symbol, entries []index.Entry) []*lsp.Symbol {
		for _, e := range entries {
			uri := canonPath(lsp.PathToURI(e.File))
			var hit *lsp.Symbol
			for _, s := range have {
				if _, l, _ := format.SymbolPosition(s); l == e.Line && canonPath(s.Location.URI) == uri {
					hit = s
					break
				}
			}
			switch {
			case hit != nil:
				if e.Constraint != "" && hit.Detail == "" {
					hit.Detail = e.Constraint
				}
			default:
				s := FromEntry(e)
				onlyIndex[s] = true
				have = append(have, s)
			}
		}
		return have
	}
	return fold(exact, ie), fold(loose, il), onlyIndex
}

// Resolve finds the one symbol named query, optionally restricted to file (workspace-relative). ix, when
// not nil, is consulted next to gopls: it knows every declaring file, built or not.
func Resolve(ctx context.Context, c *lsp.Client, ix *index.Index, root, query, file string) (Symbol, error) {
	return resolve(ctx, c, ix, root, query, file, false)
}

// ResolveExact is Resolve for tools that write: names are case-sensitive, so a name that matches only
// ignoring case ("gen" for Gen, or for a package called gen) is not found, and the error suggests the
// real spelling.
func ResolveExact(ctx context.Context, c *lsp.Client, ix *index.Index, root, query, file string) (Symbol, error) {
	return resolve(ctx, c, ix, root, query, file, true)
}

func resolve(ctx context.Context, c *lsp.Client, ix *index.Index, root, query, file string, exactCase bool) (Symbol, error) {
	query = normalizeQuery(strings.TrimSpace(query))
	last := query
	if i := strings.LastIndex(query, "."); i >= 0 {
		last = query[i+1:]
	}
	all, err := lookup(ctx, c, query, last)
	if err != nil {
		return Symbol{}, err
	}
	var exact, loose []*lsp.Symbol
	for _, s := range all {
		switch matches(s, query) {
		case 2:
			exact = append(exact, s)
		case 1:
			loose = append(loose, s)
		}
	}
	exact, loose, onlyIndex := merge(ix, query, exact, loose)
	if len(exact) == 0 && len(loose) == 0 {
		// Nothing by name: the member may be named through a type alias ("Square.Area" for a method of Sq).
		exact = aliasedMembers(all, query)
	}
	exact, loose = dedupeSymbols(format.Rank(exact, root, last)), dedupeSymbols(format.Rank(loose, root, last))
	// A case-exact match beats a merely case-insensitive one.
	cands := exact
	folded := false
	if len(cands) == 0 {
		if exactCase && len(loose) > 0 {
			var sug []string
			seen := map[string]bool{}
			for _, s := range loose {
				if f := foldedForm(s, query); f != "" && !seen[f] {
					seen[f] = true
					sug = append(sug, f)
				}
			}
			sug = closestSpellings(sug, query)
			return Symbol{}, &NotFoundError{Query: query, Suggest: sug, Err: &toolerr.InputError{Msg: fmt.Sprintf("No symbol found matching %q (names are case-sensitive).", query), Hint: sug, NotFound: true}}
		}
		cands = loose
		folded = len(loose) > 0
	}
	if query == "init" || query == "main" {
		// Every package has its own: unless one is asked for by package, only the workspace's count.
		var mine []*lsp.Symbol
		for _, s := range cands {
			if rel, err := filepath.Rel(evalLink(root), canonPath(s.Location.URI)); err == nil && !strings.HasPrefix(rel, "..") {
				mine = append(mine, s)
			}
		}
		if len(mine) > 0 {
			cands = mine
		}
	}
	if file != "" {
		var narrowed []*lsp.Symbol
		for _, exact := range []bool{true, false} {
			for _, s := range cands {
				if matchesFile(s, root, file, exact) {
					narrowed = append(narrowed, s)
				}
			}
			if len(narrowed) > 0 {
				break
			}
		}
		if len(cands) > 0 && len(narrowed) == 0 {
			fp := file
			if !filepath.IsAbs(fp) {
				fp = filepath.Join(root, fp)
			}
			if _, serr := os.Stat(fp); serr != nil {
				return Symbol{}, toolerr.Inputf("File %q does not exist in the workspace; %q is declared in %d other place(s):\n%s", file, query, len(cands), candidateLines(cands, root))
			}
			return Symbol{}, toolerr.NotFoundf("No symbol %q in %q; %d match(es) elsewhere:\n%s", query, file, len(cands), candidateLines(cands, root))
		}
		cands = narrowed
	}
	if file == "" && !exactCase { // read tools only: a write never refuses a name for its reading (NAVN-010)
		if std := qualifierAmbiguous(query, cands); std != "" {
			return Symbol{}, qualifierRefusal(query, std, cands)
		}
	}
	if len(cands) > 1 { // deterministic order for the ambiguity listing
		sort.SliceStable(cands, func(i, j int) bool {
			a, b := canonPath(cands[i].Location.URI), canonPath(cands[j].Location.URI)
			if a != b {
				return a < b
			}
			return cands[i].Location.Range.Start.Line < cands[j].Location.Range.Start.Line
		})
	}
	switch len(cands) {
	case 0:
		if !strings.Contains(query, ".") {
			if mem := memberNamed(all, query); len(mem) > 0 {
				return Symbol{}, memberRefusal(query, mem, root)
			}
		}
		hint := ""
		if !strings.Contains(query, ".") {
			hint = " (struct fields and methods are named Type.Name)"
		}
		return Symbol{}, &NotFoundError{Query: query, Err: &toolerr.InputError{Msg: fmt.Sprintf("No symbol found matching %q%s.", query, hint), NotFound: true}}
	case 1:
		sym := fromLSP(cands[0])
		sym.Unbuilt = onlyIndex[cands[0]] && cands[0].Detail != ""
		if folded {
			sym.CaseFolded = foldedForm(cands[0], query)
		}
		return sym, nil
	}
	sameFile := true
	for _, c := range cands {
		if c.Location.URI != cands[0].Location.URI {
			sameFile = false
		}
	}
	hint := "pass file_path to disambiguate"
	if q := qualifiedExample(cands[0], query); q != "" {
		hint = "qualify it with the package (`" + q + "`) or " + hint
	}
	if sameFile {
		hint = "these all live in the same file; use search_symbol to get exact line/column, then hover/definition/references with that position"
	}
	return Symbol{}, toolerr.Inputf("%d symbols match %q; %s:\n%s", len(cands), query, hint, candidateLines(cands, root))
}

// closestSpellings keeps the suggestions that differ from query in the fewest characters (the same length
// as query, as they equal it ignoring case): "cs.Foo" for "Cs.Foo", not also "cs.foo". Sorted.
func closestSpellings(sug []string, query string) []string {
	diff := func(a string) int {
		n := 0
		ra, rq := []rune(a), []rune(query)
		for i := range ra {
			if i >= len(rq) || ra[i] != rq[i] {
				n++
			}
		}
		return n
	}
	best := -1
	for _, s := range sug {
		if d := diff(s); best < 0 || d < best {
			best = d
		}
	}
	var out []string
	for _, s := range sug {
		if diff(s) == best {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
