// Package format renders LSP results as compact, agent-readable text.
// path:line:col headers, numbered snippets, ranked symbol lists, outlines, callers.
package format

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

const (
	DefaultSearchSymbolLimit      = 50
	DefaultDiagnosticsLimit       = 50
	DefaultCallersLimit           = 50
	DefaultReferenceFileLimit     = 25
	DefaultReferencesSnippetLimit = 8
)

// Relative renders uri relative to root (or the plain path when outside it).
func Relative(uri, root string) string {
	p := lsp.URIToPath(uri)
	if p == "" {
		return uri
	}
	return exclude.RelativeTo(p, root)
}

type lineKey struct {
	path  string
	mtime int64
	size  int64
}

var (
	lineMu    sync.Mutex
	lineCache = map[lineKey][]string{}
)

func readLines(p string) []string {
	st, err := os.Stat(p)
	if err != nil {
		return nil
	}
	k := lineKey{p, st.ModTime().UnixNano(), st.Size()}
	lineMu.Lock()
	defer lineMu.Unlock()
	if l, ok := lineCache[k]; ok {
		return l
	}
	b, err := os.ReadFile(p)
	if err != nil || !utf8.Valid(b) {
		return nil
	}
	text := strings.TrimSuffix(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	l := strings.Split(text, "\n")
	if len(lineCache) > 128 {
		lineCache = map[lineKey][]string{}
	}
	lineCache[k] = l
	return l
}

// snippetMaxRunes caps one source line in a snippet. A longer line is cut to a window around the match.
const snippetMaxRunes = 140

// clipLine returns at most snippetMaxRunes runes of line. The window holds utf16Col (the LSP column: 0-based,
// UTF-16 units; 0 keeps the start of the line), and "..." marks each side where text was cut.
func clipLine(line string, utf16Col int) string {
	runes := []rune(line)
	if len(runes) <= snippetMaxRunes {
		return line
	}
	idx, units := 0, 0
	for idx < len(runes) && units < utf16Col {
		if runes[idx] >= 0x10000 {
			units += 2
		} else {
			units++
		}
		idx++
	}
	lo := max(0, idx-snippetMaxRunes/2)
	hi := lo + snippetMaxRunes
	if hi > len(runes) {
		hi = len(runes)
		lo = hi - snippetMaxRunes
	}
	out := string(runes[lo:hi])
	if lo > 0 {
		out = "..." + out
	}
	if hi < len(runes) {
		out += "..."
	}
	return out
}

// Snippet is lines [start,end] (0-based, inclusive) plus context, numbered 1-based.
func Snippet(uri string, start, end, context int) string {
	return snippetAt(uri, start, end, context, -1, 0)
}

// snippetAt is Snippet with the match position: line matchLine is cut around column matchCol, the other lines
// from their start.
func snippetAt(uri string, start, end, context, matchLine, matchCol int) string {
	lines := readLines(lsp.URIToPath(uri))
	if lines == nil {
		return ""
	}
	lo, hi := max(0, start-context), min(len(lines), end+1+context)
	var out []string
	for i := lo; i < hi; i++ {
		text := lines[i]
		if i == matchLine {
			text = clipLine(text, matchCol)
		} else {
			text = clipLine(text, 0)
		}
		out = append(out, fmt.Sprintf("%5d | %s", i+1, text))
	}
	return strings.Join(out, "\n")
}

func FormatLocation(loc lsp.Location, root string) string {
	header := fmt.Sprintf("%s:%d:%d", Relative(loc.URI, root), loc.Range.Start.Line+1, loc.Range.Start.Character+1)
	if body := snippetAt(loc.URI, loc.Range.Start.Line, loc.Range.End.Line, 2, loc.Range.Start.Line, loc.Range.Start.Character); body != "" {
		return header + "\n" + body
	}
	return header
}

func FormatLocations(locs []lsp.Location, root string) string {
	parts := make([]string, len(locs))
	for i, l := range locs {
		parts[i] = FormatLocation(l, root)
	}
	return strings.Join(parts, "\n\n")
}

func KindLabel(kind int) string {
	if s, ok := lsp.SymbolKindNames[kind]; ok {
		return s
	}
	return fmt.Sprintf("Kind%d", kind)
}

// SymbolPosition is the 0-based position of a workspace symbol's name.
func SymbolPosition(s *lsp.Symbol) (uri string, line, col int) {
	if s.Location == nil {
		return "", 0, 0
	}
	if s.SelectionRange != nil {
		return s.Location.URI, s.SelectionRange.Start.Line, s.SelectionRange.Start.Character
	}
	return s.Location.URI, s.Location.Range.Start.Line, s.Location.Range.Start.Character
}

func FormatWorkspaceSymbol(s *lsp.Symbol, root string) string {
	uri, line, col := SymbolPosition(s)
	rel := "?"
	if uri != "" {
		rel = Relative(uri, root)
	}
	out := fmt.Sprintf("%s  [%s]  (%s:%d:%d)", s.Name, KindLabel(s.Kind), rel, line+1, col+1)
	if s.Detail != "" {
		out += "  build: " + s.Detail // a declaration in a file with a build constraint (from the symbol index)
	}
	return out
}

// BaseName is the last dotted component of a gopls symbol name ("shapes.Square.Area" -> "Area").
func BaseName(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

const fuzzyTier = 4

// nameFor is the part of a symbol name that a query is compared with: the last dotted component, or,
// for a dotted query ("Square.Ar"), as many trailing components as the query has.
func nameFor(name, query string) string {
	parts := strings.Split(name, ".")
	keep := min(len(parts), strings.Count(query, ".")+1)
	return strings.Join(parts[len(parts)-keep:], ".")
}

func matchTier(name, query string) int {
	name = nameFor(name, query)
	switch {
	case name == query:
		return 0
	case strings.EqualFold(name, query):
		return 1
	}
	fn, fq := strings.ToLower(name), strings.ToLower(query)
	switch {
	case strings.HasPrefix(fn, fq):
		return 2
	case strings.Contains(fn, fq):
		return 3
	}
	return fuzzyTier
}

// looseSpan judges a subsequence-only match (tier 4): the length of the stretch of name the query
// letters are spread over, and whether the match looks intended. It does when the query starts
// at the name's first letter and stays compact (span <= 3*len(query)), or when the query is part
// of the name's camel-hump initials (mgr for MaxGroupRate).
func looseSpan(name, query string) (span int, ok bool) {
	name = nameFor(name, query)
	n, q := []rune(strings.ToLower(name)), []rune(strings.ToLower(query))
	if len(q) == 0 {
		return 0, true
	}
	qi, first := 0, -1
	for i, r := range n {
		if qi < len(q) && r == q[qi] {
			if first < 0 {
				first = i
			}
			qi++
			if qi == len(q) {
				span = i - first + 1
			}
		}
	}
	if qi < len(q) {
		return 0, false
	}
	if first == 0 && span <= 3*len(q) {
		return span, true
	}
	var initials []rune
	prev := '_'
	for _, r := range name {
		if prev == '_' || (unicode.IsUpper(r) && !unicode.IsUpper(prev)) {
			initials = append(initials, unicode.ToLower(r))
		}
		prev = r
	}
	if len(q) >= 2 && strings.Contains(string(initials), string(q)) {
		return len(q), true
	}
	return 0, false
}

// dropWeak splits the weak tier-4 matches off the ranked list and orders the rest by span.
func dropWeak(ranked []*lsp.Symbol, query string) (kept, weak []*lsp.Symbol) {
	type spanned struct {
		s    *lsp.Symbol
		span int
	}
	var tail []spanned
	for _, s := range ranked {
		if matchTier(s.Name, query) < fuzzyTier {
			kept = append(kept, s)
			continue
		}
		if span, ok := looseSpan(s.Name, query); ok {
			tail = append(tail, spanned{s, span})
		} else {
			weak = append(weak, s)
		}
	}
	sort.SliceStable(tail, func(i, j int) bool { return tail[i].span < tail[j].span })
	for _, t := range tail {
		kept = append(kept, t.s)
	}
	return kept, weak
}

func IsTestPath(p string) bool { return strings.HasSuffix(p, "_test.go") }

func symFile(s *lsp.Symbol) string {
	if s.Location != nil {
		return s.Location.URI
	}
	return ""
}

// Property/Field symbols (struct fields) rank after declarations within the same tier.
var lowPriority = map[int]bool{7: true, 8: true}

// rankKey orders the symbols of an answer: the match tier first; then production code, test helpers
// (testutils, testutil, testing, fake*, mock* directories) and _test.go files, in that order; declarations
// before fields; packages outside internal/ before internal/; shallower packages first; exported names
// first. The file, line, column, name and kind settle what is left, so the order never depends on the
// order gopls answered in.
type rankKey struct {
	tier, level, field, internal, depth, unexported int
	rel, name                                       string
	line, col, kind                                 int
}

func rankKeyOf(s *lsp.Symbol, root, query string) rankKey {
	rel := Relative(symFile(s), root)
	_, line, col := SymbolPosition(s)
	k := rankKey{tier: matchTier(s.Name, query), level: testLevel(rel), rel: rel, name: s.Name, line: line, col: col, kind: s.Kind}
	if lowPriority[s.Kind] {
		k.field = 1
	}
	if dir := path.Dir(rel); dir != "." {
		parts := strings.Split(dir, "/")
		k.depth = len(parts)
		for _, p := range parts {
			if p == "internal" {
				k.internal = 1
			}
		}
	}
	if r, _ := utf8.DecodeRuneInString(BaseName(s.Name)); !unicode.IsUpper(r) {
		k.unexported = 1
	}
	return k
}

func (a rankKey) less(b rankKey) bool {
	for _, p := range [...][2]int{{a.tier, b.tier}, {a.level, b.level}, {a.field, b.field}, {a.internal, b.internal}, {a.depth, b.depth}, {a.unexported, b.unexported}} {
		if p[0] != p[1] {
			return p[0] < p[1]
		}
	}
	if a.rel != b.rel {
		return a.rel < b.rel
	}
	if a.line != b.line {
		return a.line < b.line
	}
	if a.col != b.col {
		return a.col < b.col
	}
	if a.name != b.name {
		return a.name < b.name
	}
	return a.kind < b.kind
}

// testLevel: 2 for a _test.go file, 1 for a file in a test-helper directory, 0 for the package's own code.
func testLevel(rel string) int {
	if IsTestPath(rel) {
		return 2
	}
	for _, d := range strings.Split(path.Dir(rel), "/") {
		if helperDir(d) {
			return 1
		}
	}
	return 0
}

// helperDir names the directories that hold test helpers rather than the package's own API.
func helperDir(d string) bool {
	return d == "testutils" || d == "testutil" || d == "testing" || strings.HasPrefix(d, "fake") || strings.HasPrefix(d, "mock")
}

// Rank orders symbols for an answer about query (see rankKey). root is the workspace the paths are shown
// relative to. Without a query the order is the input's.
func Rank(symbols []*lsp.Symbol, root, query string) []*lsp.Symbol {
	if query == "" {
		return append([]*lsp.Symbol(nil), symbols...)
	}
	type ranked struct {
		k rankKey
		s *lsp.Symbol
	}
	rs := make([]ranked, len(symbols))
	for i, s := range symbols {
		rs[i] = ranked{rankKeyOf(s, root, query), s}
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].k.less(rs[j].k) })
	out := make([]*lsp.Symbol, len(rs))
	for i, r := range rs {
		out[i] = r.s
	}
	return out
}

var kindByLabel = func() map[string]int {
	m := map[string]int{}
	for k, v := range lsp.SymbolKindNames {
		m[v] = k
	}
	// gopls reports Go structs as Struct and interfaces as Interface; let "type" cover both. Other named
	// types (type Color int) come as LSP Class, labelled "type"; "class" stays accepted as a filter.
	m["class"] = 5
	return m
}()

// ParseKindFilter parses "function,method" into kinds; "type" expands to struct+interface+class+enum.
func ParseKindFilter(kind string) (map[int]bool, error) {
	if strings.TrimSpace(kind) == "" {
		return nil, nil
	}
	want := map[int]bool{}
	for _, part := range strings.Split(kind, ",") {
		label := strings.ToLower(strings.TrimSpace(part))
		if label == "" {
			continue
		}
		if label == "type" {
			for _, k := range []int{5, 10, 11, 23} {
				want[k] = true
			}
			continue
		}
		k, ok := kindByLabel[label]
		if !ok {
			var valid []string
			for l := range kindByLabel {
				if l != "type" {
					valid = append(valid, l)
				}
			}
			sort.Strings(valid)
			return nil, toolerr.Inputf("unknown symbol kind %q; use one or more of: type, %s", strings.TrimSpace(part), strings.Join(valid, ", "))
		}
		want[k] = true
	}
	if len(want) == 0 {
		return nil, nil
	}
	return want, nil
}

// pathMatches: a pattern with glob characters matches as path.Match does, except that a pattern without a
// slash matches the base name in any directory and ** crosses directories. Any other pattern is a
// workspace-relative directory or file: it matches itself and what lies below it, never a sibling that
// only shares its spelling (p does not match plugins/).
func pathMatches(rel, pattern string) bool {
	pattern = strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(pattern), "\\", "/"), "./")
	if pattern == "" {
		return true
	}
	if strings.ContainsAny(pattern, "*?[") {
		if !strings.Contains(pattern, "/") {
			ok, _ := path.Match(pattern, path.Base(rel))
			return ok
		}
		if ok, _ := path.Match(pattern, rel); ok {
			return true
		}
		// path.Match has no "**": let it span directories.
		ok, _ := path.Match(strings.ReplaceAll(pattern, "**/", ""), rel)
		return ok || globStar(pattern, rel)
	}
	pattern = strings.TrimSuffix(pattern, "/")
	return rel == pattern || strings.HasPrefix(rel, pattern+"/")
}

// SearchPath turns search_symbol's path= into the workspace-relative form pathMatches compares. A path inside
// the workspace is accepted as an absolute path or as a relative one that resolves through "." or "..";
// one that resolves outside it is refused, since no symbol of the workspace can match it. "" and the
// workspace itself mean no path filter.
func SearchPath(root, p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" || root == "" {
		return p, nil
	}
	abs := filepath.Join(root, filepath.FromSlash(p))
	if path.IsAbs(p) {
		abs = filepath.Clean(filepath.FromSlash(p))
	}
	rel, err := filepath.Rel(root, abs)
	rel = filepath.ToSlash(rel)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", toolerr.Inputf("path %q is outside the workspace %s; give a path inside it, relative to the workspace (a directory such as internal/, or a glob such as **/*_test.go)", p, root)
	}
	if rel == "." {
		return "", nil
	}
	return rel, nil
}

// PathForm says how a path= filter was read, for an answer that the filter emptied.
func PathForm(p string) string {
	switch {
	case !strings.ContainsAny(p, "*?["):
		return "a workspace-relative directory or file prefix"
	case !strings.Contains(p, "/"):
		return "a glob matched against each file's base name"
	default:
		return "a glob matched against workspace-relative paths (** crosses directories)"
	}
}

func globStar(pattern, rel string) bool {
	pre, post, found := strings.Cut(pattern, "**")
	if !found {
		return false
	}
	post = strings.TrimPrefix(post, "/")
	if !strings.HasPrefix(rel, pre) {
		return false
	}
	rest := rel[len(pre):]
	for {
		if ok, _ := path.Match(post, rest); ok {
			return true
		}
		i := strings.Index(rest, "/")
		if i < 0 {
			return false
		}
		rest = rest[i+1:]
	}
}

func FilterByKindAndPath(symbols []*lsp.Symbol, root string, kinds map[int]bool, pathFilter string) []*lsp.Symbol {
	if kinds == nil && strings.TrimSpace(pathFilter) == "" {
		return symbols
	}
	var kept []*lsp.Symbol
	for _, s := range symbols {
		if kinds != nil && !kinds[s.Kind] {
			continue
		}
		if strings.TrimSpace(pathFilter) != "" && !pathMatches(Relative(symFile(s), root), pathFilter) {
			continue
		}
		kept = append(kept, s)
	}
	return kept
}

// FormatWorkspaceSymbols: ranked, capped listing of symbols that are all counted (see SearchSymbols).
func FormatWorkspaceSymbols(symbols []*lsp.Symbol, root, query string, limit int, fuzzy bool) string {
	return SearchSymbols(symbols, nil, false, root, query, limit, fuzzy)
}

// isSubsequenceOnly: the query's letters appear in the name in order, but the name does not contain the query.
func isSubsequenceOnly(s *lsp.Symbol, query string) bool {
	return matchTier(s.Name, query) == fuzzyTier && !strings.Contains(strings.ToLower(s.Name), strings.ToLower(query))
}

func anyBelowFuzzy(list []*lsp.Symbol, query string) bool {
	for _, s := range list {
		if matchTier(s.Name, query) < fuzzyTier {
			return true
		}
	}
	return false
}

func withoutFuzzy(list []*lsp.Symbol, query string) []*lsp.Symbol {
	var out []*lsp.Symbol
	for _, s := range list {
		if matchTier(s.Name, query) < fuzzyTier {
			out = append(out, s)
		}
	}
	return out
}

func countFuzzy(list []*lsp.Symbol, query string) int {
	return len(list) - len(withoutFuzzy(list, query))
}

// SearchSymbols renders the answer to search_symbol. hits are the index's exact and prefix matches: they are
// what the count is taken from. sample holds gopls's other matches, listed after the hits and filling the
// slots they leave. When partial (gopls answered with its cap, so the sample is not everything it knows), the
// sample is not counted and what it hides is not counted either. Which hits are shown or hidden depends on
// the hits only.
func SearchSymbols(hits, sample []*lsp.Symbol, partial bool, root, query string, limit int, fuzzy bool) string {
	hits = dedupe(Rank(hits, root, query))
	sample = dedupe(Rank(sample, root, query))
	if len(hits)+len(sample) == 0 {
		return ""
	}
	sampleCounted := !partial
	type item struct {
		s       *lsp.Symbol
		counted bool
	}
	var items []item
	add := func(list []*lsp.Symbol, counted bool) {
		for _, s := range list {
			items = append(items, item{s, counted})
		}
	}
	hidden, hiddenSample := 0, 0 // subsequence-only matches hidden: counted, and in the sample
	weak, weakSample := 0, 0     // matches neither prefix nor subsequence-compact: hidden in the default listing
	if query == "" {
		add(hits, true)
		add(sample, sampleCounted)
	} else {
		hasReal := anyBelowFuzzy(hits, query)
		kh, wh := dropWeak(hits, query)
		ks, ws := dropWeak(sample, query)
		switch {
		case fuzzy:
			add(kh, true)
			add(wh, true)
			add(ks, sampleCounted)
			add(ws, sampleCounted)
		case hasReal:
			add(withoutFuzzy(kh, query), true)
			add(withoutFuzzy(ks, query), sampleCounted)
			hidden, hiddenSample = countFuzzy(kh, query)+len(wh), countFuzzy(ks, query)+len(ws)
		default:
			add(kh, true)
			add(ks, sampleCounted)
			weak, weakSample = len(wh), len(ws)
		}
	}
	if sampleCounted {
		hidden, weak = hidden+hiddenSample, weak+weakSample
		hiddenSample, weakSample = 0, 0
	}

	counted := 0
	for _, it := range items {
		if it.counted {
			counted++
		}
	}
	shown := items[:min(len(items), max(0, limit))]
	lines := make([]string, 0, len(shown)+4)
	shownCounted := 0
	for _, it := range shown {
		line := FormatWorkspaceSymbol(it.s, root)
		if query != "" && isSubsequenceOnly(it.s, query) {
			line += "  (fuzzy)"
		}
		lines = append(lines, line)
		if it.counted {
			shownCounted++
		}
	}
	if omitted := counted - shownCounted; omitted > 0 {
		lines = append(lines, fmt.Sprintf("… and %d more (showing first %d); narrow with kind=… or path=…", omitted, shownCounted))
	}
	if hidden > 0 {
		s := "es"
		if hidden == 1 {
			s = ""
		}
		lines = append(lines, fmt.Sprintf("(%d looser fuzzy match%s whose names don't contain %q hidden; pass fuzzy=true to list them)", hidden, s, query))
	}
	if hiddenSample > 0 {
		lines = append(lines, fmt.Sprintf("(some looser fuzzy matches whose names don't contain %q are hidden in the partial sample; pass fuzzy=true to list them)", query))
	}
	if weak > 0 {
		what := "weak matches"
		if weak == 1 {
			what = "weak match"
		}
		lines = append(lines, fmt.Sprintf("(%d %s hidden; pass fuzzy=true to list them)", weak, what))
	}
	if weakSample > 0 {
		lines = append(lines, "(some weak matches are hidden in the partial sample; pass fuzzy=true to list them)")
	}
	if partial && len(sample) > 0 {
		lines = append(lines, fmt.Sprintf("(partial sample: gopls returns at most %d matches per query, so the fuzzy matches shown here are only a sample and are not counted; they may differ between runs)", lsp.WorkspaceSymbolCap))
	}
	return strings.Join(lines, "\n")
}

func dedupe(in []*lsp.Symbol) []*lsp.Symbol {
	seen := map[string]bool{}
	var out []*lsp.Symbol
	for _, s := range in {
		uri, l, c := SymbolPosition(s)
		k := fmt.Sprintf("%s|%s|%d|%d", s.Name, uri, l, c)
		if !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	return out
}

var severities = map[int]string{1: "error", 2: "warning", 3: "info", 4: "hint"}

func codeString(raw []byte) string {
	return strings.Trim(string(raw), `"`)
}

// FormatDiagnostic renders one diagnostic: position, [severity code] and message, then the companion locations
// of its related information, workspace-relative to root.
func FormatDiagnostic(d lsp.Diagnostic, root string) string {
	sev := severities[d.Severity]
	if sev == "" {
		sev = "?"
	}
	tag := sev
	// gopls sets the code to "default" for analyzer findings; the analyzer's name is then the source (printf).
	name := codeString(d.Code)
	if name == "" || name == "null" || name == "default" {
		name = d.Source
	}
	if name != "" {
		tag += " " + name
	}
	msg := strings.Split(d.Message, "\n")
	out := []string{fmt.Sprintf("%d:%d [%s] %s", d.Range.Start.Line+1, d.Range.Start.Character+1, tag, msg[0])}
	for _, l := range msg[1:] {
		out = append(out, "    "+l)
	}
	for _, r := range d.RelatedInformation {
		loc := fmt.Sprintf("%s:%d:%d", Relative(r.Location.URI, root), r.Location.Range.Start.Line+1, r.Location.Range.Start.Character+1)
		// gopls sends an empty message on one side of a redeclaration: the location alone is shown then.
		if r.Message != "" {
			loc += ": " + r.Message
		}
		out = append(out, "    "+loc)
	}
	return strings.Join(out, "\n")
}

// missingMetadata is gopls' wording for an import it cannot load.
var missingMetadata = regexp.MustCompile(`\(missing metadata for import of "[^"]*"\)`)

// explainCycle rewrites gopls' "missing metadata" wording when the same diagnostics also report an import cycle:
// the import cannot be loaded because it is part of that cycle. items is changed in place.
func explainCycle(items []lsp.Diagnostic) []lsp.Diagnostic {
	cycle := false
	for _, d := range items {
		if strings.Contains(d.Message, "import cycle not allowed") {
			cycle = true
			break
		}
	}
	if cycle {
		for i := range items {
			items[i].Message = missingMetadata.ReplaceAllString(items[i].Message, "(its package is part of an import cycle, so it cannot be loaded)")
		}
	}
	return items
}

// FormatDiagnostics renders diagnostics sorted by position, at most limit of them, with paths relative to root.
// The file they belong to is unknown here: see FormatDiagnosticsIn.
func FormatDiagnostics(items []lsp.Diagnostic, limit int, root string) string {
	return FormatDiagnosticsIn(items, limit, root, "")
}

// FormatDiagnosticsIn is FormatDiagnostics for the diagnostics of one file, given by its path relative to root.
// A cgo failure dump is placed on gcc's error in that file only. With file "" nothing is ever relocated, and every
// gcc error keeps its file name in the message.
func FormatDiagnosticsIn(items []lsp.Diagnostic, limit int, root, file string) string {
	items = explainCycle(append([]lsp.Diagnostic(nil), lsp.DedupeDiagnostics(lsp.CondenseCgo(file, items))...))
	if len(items) == 0 {
		return "No diagnostics."
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i].Range.Start, items[j].Range.Start
		return a.Line < b.Line || a.Line == b.Line && a.Character < b.Character
	})
	shown := items[:min(len(items), max(0, limit))]
	lines := make([]string, len(shown))
	for i, d := range shown {
		lines[i] = FormatDiagnostic(d, root)
	}
	if omitted := len(items) - len(shown); omitted > 0 {
		lines = append(lines, fmt.Sprintf("… and %d more (showing first %d)", omitted, len(shown)))
	}
	return strings.Join(lines, "\n")
}

// brokenImportRE is gopls' wording for an import path that no required module provides: the parenthetical says
// "no module", which is wrong when the module is required and its go.mod or go.sum is the problem.
var brokenImportRE = regexp.MustCompile(`^could not import (\S+) \(no required module provides package "([^"]+)"\)`)

// maxBrokenImportCauses bounds how many distinct import paths of one answer are looked up.
const maxBrokenImportCauses = 5

// ExplainBrokenImports returns copies of items whose "could not import X (no required module provides package "X")"
// parenthetical is replaced by the cause cause(X) reports, when it reports one ("" keeps the message). cause is asked
// once per distinct import path and for at most maxBrokenImportCauses paths; the input is not modified.
func ExplainBrokenImports(items []lsp.Diagnostic, cause func(importPath string) string) []lsp.Diagnostic {
	out := make([]lsp.Diagnostic, len(items))
	copy(out, items)
	looked := map[string]string{}
	for i, d := range out {
		m := brokenImportRE.FindStringSubmatch(d.Message)
		if m == nil || m[1] != m[2] {
			continue
		}
		c, ok := looked[m[1]]
		if !ok {
			if len(looked) >= maxBrokenImportCauses {
				continue
			}
			c = cause(m[1])
			looked[m[1]] = c
		}
		if c != "" {
			out[i].Message = fmt.Sprintf("could not import %s (%s)", m[1], c) + d.Message[len(m[0]):]
		}
	}
	return out
}

type pos struct{ line, col int }

// FormatReferencesGrouped: compact "path: L12, L40" grouping, no snippets.
func FormatReferencesGrouped(locs []lsp.Location, root string, fileLimit int, withColumns bool) string {
	if len(locs) == 0 {
		return "No references found."
	}
	groups := map[string]map[pos]bool{}
	for _, l := range locs {
		rel := Relative(l.URI, root)
		if groups[rel] == nil {
			groups[rel] = map[pos]bool{}
		}
		groups[rel][pos{l.Range.Start.Line + 1, l.Range.Start.Character + 1}] = true
	}
	files := make([]string, 0, len(groups))
	total := 0
	for f, ps := range groups {
		files = append(files, f)
		total += len(ps)
	}
	sort.Strings(files)
	shown := files[:min(len(files), fileLimit)]
	out := []string{fmt.Sprintf("%d reference(s) in %d file(s):", total, len(files))}
	for _, f := range shown {
		ps := make([]pos, 0, len(groups[f]))
		for p := range groups[f] {
			ps = append(ps, p)
		}
		sort.Slice(ps, func(i, j int) bool {
			if ps[i].line != ps[j].line {
				return ps[i].line < ps[j].line
			}
			return ps[i].col < ps[j].col
		})
		tags := make([]string, len(ps))
		for i, p := range ps {
			if withColumns {
				tags[i] = fmt.Sprintf("L%d:%d", p.line, p.col)
			} else {
				tags[i] = fmt.Sprintf("L%d", p.line)
			}
		}
		out = append(out, f+": "+strings.Join(tags, ", "))
	}
	if omitted := len(files) - len(shown); omitted > 0 {
		out = append(out, fmt.Sprintf("… and %d more file(s)", omitted))
	}
	return strings.Join(out, "\n")
}

// FormatReferences: snippets for a few hits, a compact listing above snippetLimit.
// Hits in _test.go files are listed under their own heading.
func FormatReferences(locs []lsp.Location, root string, snippetLimit int) string {
	if len(locs) == 0 {
		return "No references found at that position."
	}
	if len(locs) <= snippetLimit {
		return FormatLocations(locs, root)
	}
	return fmt.Sprintf("(compact list: %d > %d hits, so they are grouped by file and line with no source snippets)\n%s", len(locs), snippetLimit, FormatReferencesGrouped(locs, root, DefaultReferenceFileLimit, true))
}

// SplitTests separates locations in _test.go files from the rest.
func SplitTests(locs []lsp.Location) (prod, tests []lsp.Location) {
	for _, l := range locs {
		if IsTestPath(l.URI) {
			tests = append(tests, l)
		} else {
			prod = append(prod, l)
		}
	}
	return
}

type node struct {
	name       string
	kind       int
	start, end int
	children   []*node
}

func fromDocumentSymbol(s *lsp.Symbol) *node {
	n := &node{name: s.Name, kind: s.Kind}
	if s.Range != nil {
		n.start, n.end = s.Range.Start.Line, s.Range.End.Line
	}
	for _, c := range s.Children {
		n.children = append(n.children, fromDocumentSymbol(c))
	}
	sort.SliceStable(n.children, func(i, j int) bool { return n.children[i].start < n.children[j].start })
	return n
}

var methodNameRE = regexp.MustCompile(`^\(\*?([^)]+)\)\.`)

// FormatOutline: indented "name  [Kind]  :start-end" tree from hierarchical document symbols.
func FormatOutline(symbols []*lsp.Symbol) string { return FormatOutlineGroups(symbols, nil) }

// FormatOutlineGroups is FormatOutline with const/var/type blocks shown as a header line over
// their (indented) members. gopls names methods (T).M or (*T).M; they are shown as T.M like the
// other tools do.
func FormatOutlineGroups(symbols []*lsp.Symbol, groups []gosrc.Group) string {
	if len(symbols) == 0 {
		return "No symbols found."
	}
	var roots []*node
	for _, s := range symbols {
		roots = append(roots, fromDocumentSymbol(s))
	}
	sort.SliceStable(roots, func(i, j int) bool { return roots[i].start < roots[j].start })
	var lines []string
	var walk func(ns []*node, depth int)
	walk = func(ns []*node, depth int) {
		cur := -1 // index of the group whose header was written last
		for _, n := range ns {
			if depth == 0 {
				gi := -1
				for i, g := range groups {
					if n.start >= g.Start && n.start <= g.End {
						gi = i
					}
				}
				if gi >= 0 && gi != cur {
					g := groups[gi]
					tags := "group"
					if g.Iota {
						tags += ", iota"
					}
					lines = append(lines, fmt.Sprintf("%s (…)  [%s]  :%d-%d", g.Keyword, tags, g.Start+1, g.End+1))
				}
				cur = gi
			}
			indent := depth
			if depth == 0 && cur >= 0 {
				indent++
			}
			span := fmt.Sprintf(":%d", n.start+1)
			if n.end != n.start {
				span = fmt.Sprintf(":%d-%d", n.start+1, n.end+1)
			}
			lines = append(lines, fmt.Sprintf("%s%s  [%s]  %s", strings.Repeat("  ", indent), methodNameRE.ReplaceAllString(n.name, "$1."), KindLabel(n.kind), span))
			// Children of funcs/vars/consts are locals, not structure.
			switch n.kind {
			case 6, 9, 12, 13, 14:
			default:
				walk(n.children, indent+1)
			}
		}
	}
	walk(roots, 0)
	return strings.Join(lines, "\n")
}

// FormatCallers lists the callers. recvs maps a call's index to the receiver type of the method
// that makes it (gopls reports methods as plain functions).
func FormatCallers(calls []lsp.IncomingCall, root string, recvs map[int]string) string {
	if len(calls) == 0 {
		return "No callers found."
	}
	total := len(calls)
	if total > DefaultCallersLimit {
		calls = calls[:DefaultCallersLimit]
	}
	lines := make([]string, len(calls), len(calls)+1)
	for i, c := range calls {
		callerLine := c.From.SelectionRange.Start.Line + 1
		seen := map[int]bool{}
		var nums []int
		for _, r := range c.FromRanges {
			if n := r.Start.Line + 1; !seen[n] {
				seen[n] = true
				nums = append(nums, n)
			}
		}
		sort.Ints(nums)
		sites := make([]string, len(nums))
		for j, n := range nums {
			sites[j] = fmt.Sprintf("L%d", n)
		}
		site := strings.Join(sites, ", ")
		if site == "" {
			site = fmt.Sprintf("L%d", callerLine)
		}
		name, kind := c.From.Name, KindLabel(c.From.Kind)
		switch recv := recvs[i]; {
		case recv != "":
			name, kind = recv+"."+name, "method"
		case name == "init" && c.From.Kind == 13:
			kind = "var initializer"
		}
		lines[i] = fmt.Sprintf("%s  [%s]  (%s:%d) calls at %s", name, kind, Relative(c.From.URI, root), callerLine, site)
	}
	if total > len(calls) {
		lines = append(lines, fmt.Sprintf("… and %d more caller(s)", total-len(calls)))
	}
	return strings.Join(lines, "\n")
}
