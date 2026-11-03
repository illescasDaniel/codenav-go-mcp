// Package tools holds the MCP tool handlers.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/illescasDaniel/codenav-go-mcp/internal/buildscope"
	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
	"github.com/illescasDaniel/codenav-go-mcp/internal/format"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
	"github.com/illescasDaniel/codenav-go-mcp/internal/goplscmd"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
	"github.com/illescasDaniel/codenav-go-mcp/internal/index"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/notices"
	"github.com/illescasDaniel/codenav-go-mcp/internal/params"
	"github.com/illescasDaniel/codenav-go-mcp/internal/resolve"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
	"github.com/illescasDaniel/codenav-go-mcp/internal/txn"
)

// Workspace is a started client plus the root it serves.
type Workspace struct {
	Client       *lsp.Client
	Index        *index.Index // declarations of every .go file below Root (nil in tests)
	Root, Source string
	Journal      *txn.Journal
	Store        *txn.PlanStore
}

// Host supplies the workspace for a call.
type Host interface {
	Acquire(ctx context.Context, sess *mcp.ServerSession) (*Workspace, error)
	Notes() *notices.Board
	Explain(source string) string
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// add registers a read-only text tool; expected errors become tool text (isError set, see finishResult).
func add[In any](s *mcp.Server, h Host, name, desc string, fn func(ctx context.Context, w *Workspace, in In) (string, error)) {
	registerUsage[In](name)
	t := &mcp.Tool{Name: name, Description: desc, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)}}
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		w, err := h.Acquire(ctx, req.Session)
		var out string
		if err == nil {
			if err = w.checkReadable(filePathOf(in)); err == nil {
				out, err = fn(ctx, w, in)
			}
		}
		res, rerr := finishResult(ctx, h, w, false, out, err)
		return res, nil, rerr
	})
}

// finishResult turns a handler's outcome into the tool result, the one place every answer passes: an
// unexpected error is returned as is; the text is shown with workspace-relative paths and a bounded size,
// plus the broken-configuration warning (not for plain argument mistakes) and pending notices. A refusal or
// a failure sets isError; a valid query with no match does not (toolerr.InputError.NotFound), except in the
// write tools (write), where it is a refused change.
func finishResult(ctx context.Context, h Host, w *Workspace, write bool, out string, err error) (*mcp.CallToolResult, error) {
	trivial, failed := false, false
	if err != nil {
		t, ok := toolerr.Format(err)
		if !ok {
			return nil, err
		}
		out, trivial = t, toolerr.IsTrivial(err)
		failed = write || !toolerr.IsNotFound(err)
	}
	if w != nil {
		out = w.scrub(out)
	}
	out = capOutput(out)
	if !trivial {
		out = w.withConfigProblem(ctx, out)
	}
	res := text(h.Notes().Annotate(out))
	res.IsError = failed
	return res, nil
}

// maxOutputBytes bounds one answer; the rest is dropped with a note saying how much.
const maxOutputBytes = 400_000

func capOutput(out string) string {
	if len(out) <= maxOutputBytes {
		return out
	}
	cut := strings.LastIndex(out[:maxOutputBytes], "\n")
	if cut < maxOutputBytes/2 {
		cut = maxOutputBytes
		// Never cut inside a multi-byte character: back up to its first byte.
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
	}
	return fmt.Sprintf("%s\n... %d more byte(s) omitted (the answer is over %d bytes; narrow the request).", out[:cut], len(out)-cut, maxOutputBytes)
}

// withConfigProblem appends the go command's complaint about a broken go.mod / go.work, which otherwise
// shows up as unrelated "nothing found" answers (gopls loads no packages at all).
func (w *Workspace) withConfigProblem(ctx context.Context, out string) string {
	if w == nil {
		return out
	}
	if p := gomod.Health(ctx, w.Root); p != "" {
		return out + "\n\nWarning: the Go configuration of this workspace is broken, so no package is loaded and type-resolved results (symbols, diagnostics, rename) are empty or wrong: " + p
	}
	return out
}

// localHint is added to the not-found answer: the index holds no local variables or parameters.
const localHint = " Local variables and parameters are not in the symbol index; use hover, definition or references at a use of the name."

// searchQuery is the name search_symbol looks for: query, or its alias name. Two different values are refused,
// since one of them would be ignored.
func searchQuery(query, name string) (string, error) {
	if query != "" && name != "" && query != name {
		return "", toolerr.Inputf("query and name are aliases; pass one value (query=%q, name=%q)", query, name)
	}
	return params.ResolveName("CreateUser", params.Alias{Name: "query", Value: query}, params.Alias{Name: "name", Value: name})
}

// indexHits are the index's exact and prefix matches of q. The index reads every .go file below the root
// whatever the build configuration (up to index.MaxFiles), so these are never capped or missed; they are
// what a search_symbol answer is counted from.
func indexHits(ix *index.Index, q string) []*lsp.Symbol {
	if ix == nil {
		return nil
	}
	var out []*lsp.Symbol
	for _, e := range ix.Prefix(q) {
		if s := resolve.FromEntry(e); !exclude.IsSyntheticName(s.Name) {
			out = append(out, s)
		}
	}
	return out
}

// searchAnswer is the search_symbol answer for q. syms is gopls's workspace/symbol list, partial when gopls
// answered with its cap (so it may not hold every fuzzy match). A gopls match whose file and line the index
// already reports is the index's; the rest are the sample. kinds and pathF are the parsed kind filter and
// the normalised path filter.
func searchAnswer(root string, ix *index.Index, q string, syms []lsp.Symbol, partial bool, kinds map[int]bool, pathF string, fuzzy bool) string {
	hits := indexHits(ix, q)
	type declKey struct {
		uri  string
		line int
	}
	seen := map[declKey]bool{}
	for _, s := range hits {
		uri, l, _ := format.SymbolPosition(s)
		seen[declKey{uri, l}] = true
	}
	ptrs := make([]*lsp.Symbol, len(syms))
	for i := range syms {
		ptrs[i] = &syms[i]
	}
	var sample []*lsp.Symbol
	for _, s := range dropNoise(ptrs) {
		if uri, l, _ := format.SymbolPosition(s); !seen[declKey{uri, l}] {
			sample = append(sample, s)
		}
	}
	note := ""
	if ix != nil && ix.Capped() {
		note = fmt.Sprintf("\n(the symbol index holds only the first %d .go files below the workspace root; declarations in the rest are not listed, so the counts are lower bounds)", index.MaxFiles)
	}
	if len(hits)+len(sample) == 0 {
		return fmt.Sprintf("No symbols matching %q.", shortQuery(q)) + localHint + note
	}
	kh := format.FilterByKindAndPath(hits, root, kinds, pathF)
	ks := format.FilterByKindAndPath(sample, root, kinds, pathF)
	if len(kh)+len(ks) == 0 {
		total := len(hits)
		if !partial {
			total += len(sample)
		}
		msg := fmt.Sprintf("No symbols matching %q with the given kind/path filters (%d without them).", shortQuery(q), total)
		if pathF != "" {
			msg += fmt.Sprintf(" The path %q was read as %s.", pathF, format.PathForm(pathF))
		}
		return msg + note
	}
	body := format.SearchSymbols(kh, ks, partial, root, q, format.DefaultSearchSymbolLimit, fuzzy)
	if onlyNotes(body) {
		// Only notes are left, the weak matches hidden: nothing matched, and a local variable is the usual reason.
		return fmt.Sprintf("No symbols matching %q.", shortQuery(q)) + localHint + "\n" + body + note
	}
	return body + note
}

// onlyNotes reports whether a rendered answer has no symbol line: every line is a parenthesised note or an
// ellipsis line, and a symbol line never starts with either.
func onlyNotes(body string) bool {
	if body == "" {
		return false
	}
	for _, l := range strings.Split(body, "\n") {
		if l != "" && !strings.HasPrefix(l, "(") && !strings.HasPrefix(l, "…") {
			return false
		}
	}
	return true
}

// isCgoGenerated: a file cgo wrote into the go build cache.
func isCgoGenerated(p string) bool { return exclude.IsCgoGenerated(p) }

// gopls's hover is markdown, which writes the underscores of such a name as `\_Ctype\_int`.
var cgoNameRE = regexp.MustCompile(`(?:\b_|\\_)C(?:func|type|var|const|macro)\\?_((?:\w|\\_)+)`)

// cgoHover shows cgo's generated names the way the source spells them: `_Cfunc_add` is `C.add`.
func cgoHover(path, text string) string {
	if !strings.Contains(text, "_C") {
		return text
	}
	if src, _, err := edits.ReadSource(path); err != nil || !strings.Contains(src, `import "C"`) {
		return text
	}
	return cgoNameRE.ReplaceAllString(text, "C.$1")
}

// maxDefinitions caps the locations `definition` lists.
const maxDefinitions = 10

// cgoSelectorName moves a position on the `C` or the `.` of `C.x` onto x (gopls has no hover for the
// qualifier of a cgo name); any other position is returned as it is.
func cgoSelectorName(w *Workspace, in posIn) posIn {
	abs := in.FilePath
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(w.Root, abs)
	}
	src, _, err := edits.ReadSource(abs)
	if err != nil || !strings.Contains(src, `import "C"`) {
		return in
	}
	off := edits.Offset(src, in.Line-1, in.Column-1)
	switch {
	case strings.HasPrefix(src[off:], "C."):
		off += 2
	case off > 0 && strings.HasPrefix(src[off-1:], "C."):
		off++
	default:
		return in
	}
	line, col := edits.PositionOf(src, off)
	in.Line, in.Column = line+1, col+1
	return in
}

// cgoDefinition answers `definition` on the X of `C.X` in a file importing "C": gopls would point into
// generated code in the build cache. It names the line of the cgo preamble that declares X, or says
// where X must live. "" when the position is not a C.X reference.
func cgoDefinition(w *Workspace, in posIn) string {
	abs := in.FilePath
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(w.Root, abs)
	}
	src, _, err := edits.ReadSource(abs)
	if err != nil || !strings.Contains(src, `import "C"`) {
		return ""
	}
	name := identAt(abs, in.Line-1, in.Column-1)
	if name == "" {
		return ""
	}
	off := edits.Offset(src, in.Line-1, in.Column-1)
	for off > 0 && (src[off-1] == '_' || src[off-1] >= '0' && src[off-1] <= '9' || src[off-1] >= 'a' && src[off-1] <= 'z' || src[off-1] >= 'A' && src[off-1] <= 'Z') {
		off--
	}
	if off < 2 || src[off-2:off] != "C." || (off > 2 && (src[off-3] == '_' || src[off-3] >= 'a' && src[off-3] <= 'z' || src[off-3] >= 'A' && src[off-3] <= 'Z' || src[off-3] >= '0' && src[off-3] <= '9')) {
		return ""
	}
	pre := src[:strings.Index(src, `import "C"`)]
	word := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
	call := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\s*\(`)
	best := 0
	for i, l := range strings.Split(pre, "\n") {
		if call.MatchString(l) {
			best = i + 1
			break
		}
		if best == 0 && word.MatchString(l) && !strings.HasPrefix(strings.TrimSpace(l), "#include") {
			best = i + 1
		}
	}
	rel := edits.RelativeName(abs, w.Root)
	if best > 0 {
		return fmt.Sprintf("C.%s is C code, declared in the cgo preamble: %s:%d\n    %s", name, rel, best, strings.TrimSpace(strings.Split(pre, "\n")[best-1]))
	}
	return fmt.Sprintf("C.%s is a C symbol with no Go declaration; look for it in the headers included by the cgo preamble of %s (or in a .c/.h file of the package).", name, rel)
}

// dropGeneratedLocs removes locations in files cgo generated into the build cache.
func dropGeneratedLocs(locs []lsp.Location) []lsp.Location {
	kept := locs[:0:0]
	for _, l := range locs {
		if !isCgoGenerated(lsp.URIToPath(l.URI)) {
			kept = append(kept, l)
		}
	}
	return kept
}

// dropGeneratedCalls removes callers that cgo generated (`_cgoexp_<hash>_goCallback` calls an exported
// function from the build cache): synthetic names and go-build cache locations.
func dropGeneratedCalls(calls []lsp.IncomingCall) []lsp.IncomingCall {
	kept := calls[:0:0]
	for _, c := range calls {
		if !exclude.IsSyntheticName(c.From.Name) && !isCgoGenerated(lsp.URIToPath(c.From.URI)) {
			kept = append(kept, c)
		}
	}
	return kept
}

// dropNoise removes symbols from files the go tool never compiles (`_x.go`, `//go:build ignore`) and
// the generated ones cgo leaves in the build cache.
func dropNoise(syms []*lsp.Symbol) []*lsp.Symbol {
	cache := exclude.GoCache()
	unbuildable := map[string]bool{}
	kept := syms[:0:0]
	for _, s := range syms {
		uri, _, _ := format.SymbolPosition(s)
		p := lsp.URIToPath(uri)
		if p != "" {
			if cache != "" && strings.HasPrefix(p, cache+string(filepath.Separator)) || strings.Contains(filepath.ToSlash(p), "/go-build/") {
				continue
			}
			u, ok := unbuildable[p]
			if !ok {
				u = index.Unbuildable(p)
				unbuildable[p] = u
			}
			if u {
				continue
			}
		}
		if !exclude.IsSyntheticName(s.Name) {
			kept = append(kept, s)
		}
	}
	return kept
}

// uncoveredNote says why callers and references may miss users when the symbol lives in a module the
// workspace's go.mod/go.work does not cover (a replace-d directory, a nested module): gopls holds it as a
// separate folder, so packages of other modules importing it are not in its view. "" otherwise.
func uncoveredNote(w *Workspace, sym resolve.Symbol) string {
	p := lsp.URIToPath(sym.URI)
	if p == "" {
		return ""
	}
	n, ok := gomod.Uncovered(w.Root, p)
	if !ok {
		return ""
	}
	name, rel := n.Path, edits.RelativeName(n.Dir, w.Root)
	if name == "" {
		name = rel
	}
	return fmt.Sprintf("Note: %s is in module %s (%s/go.mod), which the workspace's go.mod/go.work does not cover (a nested or replace-d module); gopls holds it as a separate folder, so callers and references in other modules, such as the root module, are not seen. A go.work listing the modules (go work init . ./%s) joins them.", sym.Name, name, rel, rel)
}

// caseNote tells that a read tool matched name only ignoring case (write tools refuse such a name).
func caseNote(w *Workspace, name string, sym resolve.Symbol) string {
	if sym.CaseFolded == "" {
		return ""
	}
	msg := fmt.Sprintf("Note: no symbol is called %q (names are case-sensitive); this is %s.", name, sym.CaseFolded)
	if dir := packageDir(w.Root, name); dir != "" {
		msg += fmt.Sprintf(" %s is also a package (directory %s).", name, dir)
	}
	return msg
}

// checkGoFile refuses a path that is not a .go file. A path that does not exist is reported as missing first,
// whatever its extension (so "p/p.go " says File not found, not "not a .go file").
func checkGoFile(w *Workspace, p string) error {
	if !strings.HasSuffix(p, ".go") {
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(w.Root, abs)
		}
		if _, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) {
			return &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
		}
		return toolerr.Inputf("%q is not a .go file.", p)
	}
	return nil
}

type posIn struct {
	FilePath string `json:"file_path" jsonschema:"file path, relative to the workspace root"`
	Line     int    `json:"line" jsonschema:"1-indexed line"`
	Column   int    `json:"column" jsonschema:"1-indexed UTF-16 character offset on the line"`
}

type refsIn struct {
	posIn
	IncludeDeclaration *bool `json:"include_declaration,omitempty"`
}

type nameIn struct {
	Name     string `json:"name,omitempty" jsonschema:"symbol name, e.g. Square.Area or shapes.Scale"`
	Query    string `json:"query,omitempty" jsonschema:"alias for name"`
	FilePath string `json:"file_path,omitempty" jsonschema:"disambiguates a common name"`
}

type symInfoIn struct {
	nameIn
	IncludeReferences *bool `json:"include_references,omitempty"`
}

type searchIn struct {
	Query string `json:"query,omitempty"`
	Name  string `json:"name,omitempty" jsonschema:"alias for query"`
	Kind  string `json:"kind,omitempty" jsonschema:"comma-separated kinds: function,method,interface,struct,type,..."`
	Path  string `json:"path,omitempty" jsonschema:"workspace-relative directory or file prefix (internal/ matches internal/...; p does not match plugins/), or a glob (*_test.go matches the base name in any directory; **/*_test.go crosses directories); an absolute path inside the workspace is accepted"`
	Fuzzy bool   `json:"fuzzy,omitempty"`
}

type fileIn struct {
	FilePath string `json:"file_path"`
}

func hoverText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var m struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(raw, &m) == nil && m.Value != "" {
		return m.Value
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		parts := make([]string, 0, len(arr))
		for _, a := range arr {
			if t := hoverText(a); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// maxHoverBytes caps what hover and symbol_info print; a package's hover can be tens of KB.
const maxHoverBytes = 4096

// adHocLink matches the pkg.go.dev link gopls prints for files outside any package (a `package main`
// scratch file): the address is made up and leads nowhere.
// fileLink: the file:// URLs gopls puts into stdlib doc links; the plain path reads better.
var fileLink = regexp.MustCompile(`file://[^\s)\]>"]+`)

// A workspace without a go.mod makes gopls print `pkg.go.dev/_/<absolute dir>#Name` (the path of a GOPATH-style
// directory package): it leaks the directory and leads nowhere.
// A replaced module (directory) has no version, so gopls prints `pkg.go.dev/../dir@#Name`; those go too.
// So do links to a dotted import path without a version (example.com/x#Name): that is a module of the
// workspace itself (the main module, a go.work "use", a local replace), which pkg.go.dev does not know.
// The stdlib (no dot in the first element) and versioned module-cache dependencies keep theirs.
var adHocLink = regexp.MustCompile(`(?m)\n*(?:---\n+)?\[[^\]\n]*\]\(https://pkg\.go\.dev/(?:command-line-arguments[^)\n]*|_/[^)\n]*|[./][^)\n]*|[^)\s]*@[#/][^)\n]*|[^)\s]*@|[^/)\s@]*\.[^/)\s@]*(?:/[^)\s@]*)?)\)[ \t]*`)

// dotlessLink matches a pkg.go.dev link whose import path starts with an element that has no dot (the first
// capture). pkg.go.dev serves the standard library under such names and does not serve a dotless module path.
var dotlessLink = regexp.MustCompile(`(?m)\n*(?:---\n+)?\[[^\]\n]*\]\(https://pkg\.go\.dev/([^/)\s@#]+)(?:/[^)\s@#]*)?(?:#[^)\s@]*)?\)[ \t]*`)

// dropDotlessModuleLinks removes the links to a dotless module path (`myapp/x#X`) and keeps the standard
// library's (`fmt#Println`): an element with no dot that is a directory of the Go root is the standard library.
func dropDotlessModuleLinks(t string) string {
	return dotlessLink.ReplaceAllStringFunc(t, func(m string) string {
		sub := dotlessLink.FindStringSubmatch(m)
		if sub == nil || strings.Contains(sub[1], ".") || stdlibTop(sub[1]) {
			return m
		}
		return ""
	})
}

// tidyHover drops useless links and caps the length, keeping the head (signature and summary).
func tidyHover(t string) string {
	t = strings.TrimRight(dropDotlessModuleLinks(adHocLink.ReplaceAllString(t, "")), "\n ")
	t = fileLink.ReplaceAllStringFunc(t, func(m string) string {
		if u, err := url.PathUnescape(strings.TrimPrefix(m, "file://")); err == nil {
			return u
		}
		return m
	})
	if len(t) <= maxHoverBytes {
		return t
	}
	cut := maxHoverBytes
	for cut > 0 && !utf8.RuneStart(t[cut]) {
		cut--
	}
	if i := strings.LastIndex(t[:cut], "\n"); i > maxHoverBytes/2 {
		cut = i
	}
	return t[:cut] + fmt.Sprintf("\n\n(truncated, %d more bytes)", len(t)-cut)
}

// notCompiledErr is the refusal for a file the go tool never compiles (`_x.go`, `//go:build ignore`).
func notCompiledErr(file string) error {
	return toolerr.Inputf("%s is not part of any package the Go toolchain builds (build constraints / name prefix); nothing to analyse.", file)
}

// navErr is friendlyNavErr for the read tools. When the Go configuration is broken (go.mod, go.work, GOWORK),
// gopls has no package for the file and its "no package" / "no views" answers are not about build constraints
// or name prefixes: the answer names the configuration problem instead.
func (w *Workspace) navErr(ctx context.Context, err error, file string, line, col int) error {
	var re *lsp.RequestError
	if errors.As(err, &re) && (strings.Contains(re.Message, "no package metadata") || strings.Contains(re.Message, "no views")) {
		if p := gomod.Health(ctx, w.Root); p != "" {
			return toolerr.Inputf("%s cannot be analysed: the Go configuration is broken: %s", file, p)
		}
	}
	return friendlyNavErr(err, file, line, col)
}

// friendlyNavErr turns the language server's errors about a position or file that has no analysis into
// plain sentences.
func friendlyNavErr(err error, file string, line, col int) error {
	var re *lsp.RequestError
	if !errors.As(err, &re) {
		return err
	}
	switch {
	case strings.Contains(re.Message, "no package metadata"):
		return notCompiledErr(file)
	case strings.Contains(re.Message, "no identifier found"):
		return toolerr.NotFoundf("No identifier at %s:%d:%d.", file, line, col)
	}
	return err
}

// siblingSyntaxNote names the first sibling file of abs in the same build that does not parse. gopls type-checks
// the package on the parser's partial trees, so a clean file can report "undefined: X" for a name declared in the
// broken file; the note points at the syntax error to fix first. A _test.go sibling counts only for a _test.go
// file, since the package a non-test file is built into does not contain test files. src is abs's text; "" when
// abs itself does not parse (gopls reports that error) or no sibling fails.
func siblingSyntaxNote(root, abs, src string) string {
	if _, err := parser.ParseFile(token.NewFileSet(), abs, src, parser.SkipObjectResolution); err != nil {
		return ""
	}
	entries, err := os.ReadDir(filepath.Dir(abs))
	if err != nil {
		return ""
	}
	self := filepath.Base(abs)
	build := buildscope.Context()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == self || !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") && !strings.HasSuffix(self, "_test.go") {
			continue
		}
		p := filepath.Join(filepath.Dir(abs), name)
		if _, excluded := buildscope.Excluded(build, p); excluded {
			continue
		}
		text, _, rerr := edits.ReadSource(p)
		if rerr != nil {
			continue
		}
		_, perr := parser.ParseFile(token.NewFileSet(), p, text, parser.SkipObjectResolution)
		if perr == nil {
			continue
		}
		at := exclude.RelativeTo(p, root)
		var list scanner.ErrorList
		if errors.As(perr, &list) && len(list) > 0 {
			at = fmt.Sprintf("%s:%d:%d", at, list[0].Pos.Line, list[0].Pos.Column)
		}
		return fmt.Sprintf("(note: package has a syntax error in %s; type errors here such as undefined names may be consequences of it, so fix that first)", at)
	}
	return ""
}

// ownSyntaxNote gives the first syntax error of abs itself, for an answer about that file (outline, hover,
// definition): such an answer can be partial or "nothing here" for a valid identifier. "" when abs parses or
// cannot be read.
func ownSyntaxNote(w *Workspace, abs string) string {
	text, _, err := edits.ReadSource(abs)
	if err != nil {
		return ""
	}
	_, perr := parser.ParseFile(token.NewFileSet(), abs, text, parser.SkipObjectResolution)
	if perr == nil {
		return ""
	}
	at := exclude.RelativeTo(abs, w.Root)
	var list scanner.ErrorList
	if errors.As(perr, &list) && len(list) > 0 {
		return fmt.Sprintf("%s:%d:%d: syntax error: %s (the file does not parse, so the answer may be incomplete)", at, list[0].Pos.Line, list[0].Pos.Column, list[0].Msg)
	}
	return fmt.Sprintf("%s: %v (the file does not parse, so the answer may be incomplete)", at, perr)
}

// withNote puts a note line before an answer ("" note: the answer as it is).
func withNote(note, out string) string {
	if note == "" {
		return out
	}
	return note + "\n" + out
}

// excludedNote says that the build leaves out the file subject (a sentence start, such as "This file"), and why
// (reason: buildscope's). gopls checks such a file on its own, so the errors it shows may not match the build.
func excludedNote(subject, reason string) string {
	if buildscope.NeverBuilt(reason) {
		return fmt.Sprintf("%s has %s, so no build includes it: it is a standalone program; run it with go run.", subject, reason)
	}
	return fmt.Sprintf("%s is excluded from the current build (%s); gopls checked it on its own, so errors that depend on the other files of its package may be missing. To include it, start the server with the GOOS or GOARCH it needs (GOOS=windows for a _windows file), or add its tag to -tags in CODENAV_GO_MCP_BUILD_FLAGS.", subject, reason)
}

// excludedUsesNote names the uses of a package-level func in files the build leaves out: gopls' callers cannot list
// them, so a "No callers found." answer needs this. "" when there are none, or sym is a method (its selectors are
// not told apart by name).
func excludedUsesNote(w *Workspace, sym resolve.Symbol) string {
	path := lsp.URIToPath(sym.URI)
	text, _, err := edits.ReadSource(path)
	if err != nil {
		return ""
	}
	shape, ok := declShapeAt(path, text, edits.Offset(text, sym.Line, sym.Column))
	name := identAt(path, sym.Line, sym.Column)
	if !ok || shape.method || name == "" {
		return ""
	}
	dir := filepath.Dir(path)
	ip := ""
	if m, err := moduleOf(dir); err == nil {
		ip, _ = importPathOf(m, dir)
	}
	var uses []string
	for _, h := range buildscope.Mentions(buildscope.Context(), buildscope.Query{Root: w.Root, DeclFile: path, Name: name, ImportPath: ip}) {
		if h.Kind == buildscope.Use {
			uses = append(uses, fmt.Sprintf("%s:%d (%s)", edits.RelativeName(h.Path, w.Root), h.Line, h.Reason))
		}
	}
	if len(uses) == 0 {
		return ""
	}
	return fmt.Sprintf("%d use(s) of %s in files the build leaves out are not in this list: %s. To include them, start the server with the GOOS or GOARCH they need, or add their tags to -tags in CODENAV_GO_MCP_BUILD_FLAGS.", len(uses), name, listing(uses))
}

// RegisterNav registers the navigation tools.
func RegisterNav(s *mcp.Server, h Host) {
	add(s, h, "workspace", "Which directory is codenav navigating, and why? Use when results look like they come from the wrong checkout/worktree.",
		func(ctx context.Context, w *Workspace, _ struct{}) (string, error) {
			return workspaceReport(ctx, h, w), nil
		})

	add(s, h, "hover", "Type/documentation info for the symbol at a position. line/column are 1-indexed; column is a UTF-16 offset.",
		func(ctx context.Context, w *Workspace, in posIn) (string, error) {
			if err := checkGoFile(w, in.FilePath); err != nil {
				return "", err
			}
			raw, err := w.Client.Hover(ctx, in.FilePath, in.Line, in.Column)
			if err != nil {
				// gopls has nothing for C.x (no package data for the cgo file): say what C.x is, as definition does.
				if msg := cgoDefinition(w, cgoSelectorName(w, in)); msg != "" {
					return msg, nil
				}
				return "", w.navErr(ctx, err, in.FilePath, in.Line, in.Column)
			}
			abs := in.FilePath
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(w.Root, abs)
			}
			note := ownSyntaxNote(w, abs)
			if t := tidyHover(hoverText(raw)); t != "" {
				return withNote(note, cgoHover(abs, t)), nil
			}
			return withNote(note, "No hover information at that position."), nil
		})

	add(s, h, "definition", "Go to the definition of the symbol at a position (type-resolved).",
		func(ctx context.Context, w *Workspace, in posIn) (string, error) {
			if err := checkGoFile(w, in.FilePath); err != nil {
				return "", err
			}
			if msg := cgoDefinition(w, in); msg != "" {
				return msg, nil
			}
			locs, err := w.Client.Definition(ctx, in.FilePath, in.Line, in.Column)
			if err != nil {
				return "", w.navErr(ctx, err, in.FilePath, in.Line, in.Column)
			}
			abs := in.FilePath
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(w.Root, abs)
			}
			note := ownSyntaxNote(w, abs)
			if len(locs) == 0 {
				return withNote(note, "No definition found at that position."), nil
			}
			more := 0
			if len(locs) > maxDefinitions {
				// An import path has one location per file of the package.
				more = len(locs) - maxDefinitions
				locs = locs[:maxDefinitions]
			}
			out := format.FormatLocations(locs, w.Root)
			if more > 0 {
				out += fmt.Sprintf("\n(+%d more files)", more)
			}
			return withNote(note, out), nil
		})

	add(s, h, "references", "All usages of the symbol at a position across the workspace. Hits in _test.go files are listed separately.",
		func(ctx context.Context, w *Workspace, in refsIn) (string, error) {
			if err := checkGoFile(w, in.FilePath); err != nil {
				return "", err
			}
			decl := in.IncludeDeclaration == nil || *in.IncludeDeclaration
			locs, err := w.Client.References(ctx, in.FilePath, in.Line, in.Column, decl)
			if err != nil {
				return "", w.navErr(ctx, err, in.FilePath, in.Line, in.Column)
			}
			return referencesAnswer(dropGeneratedLocs(locs), w.Root, toolchainLocs(w.Root)), nil
		})

	add(s, h, "search_symbol", "Search the workspace for a symbol by name. Returns positions to feed to definition/references/hover. Production code ranks before tests.",
		func(ctx context.Context, w *Workspace, in searchIn) (string, error) {
			kinds, err := format.ParseKindFilter(in.Kind)
			if err != nil {
				return "", err
			}
			q, err := searchQuery(in.Query, in.Name)
			if err != nil {
				return "", err
			}
			pathF, err := format.SearchPath(w.Root, in.Path)
			if err != nil {
				return "", err
			}
			syms, partial, err := w.Client.WorkspaceSymbolCapped(ctx, q)
			if err != nil {
				return "", err
			}
			return searchAnswer(w.Root, w.Index, q, syms, partial, kinds, pathF, in.Fuzzy), nil
		})

	add(s, h, "diagnostics", "gopls diagnostics (errors/warnings) for a single file.",
		func(ctx context.Context, w *Workspace, in fileIn) (string, error) {
			if err := checkGoFile(w, in.FilePath); err != nil {
				return "", err
			}
			abs := in.FilePath
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(w.Root, abs)
			}
			rel, err := filepath.Rel(w.Root, abs)
			if err != nil || strings.HasPrefix(rel, "..") {
				return "", toolerr.Inputf("%s is outside the workspace: diagnostics only cover files of the workspace's own modules. Files of the standard library, the module cache and replaced modules elsewhere can be read (outline, hover, references) but are not type-checked as part of this workspace.", in.FilePath)
			}
			items, err := w.Client.Diagnostics(ctx, in.FilePath)
			if err != nil {
				return "", w.navErr(ctx, err, in.FilePath, 0, 0)
			}
			out := format.FormatDiagnosticsIn(items, format.DefaultDiagnosticsLimit, w.Root, filepath.ToSlash(rel))
			if src, _, rerr := edits.ReadSource(abs); rerr == nil {
				if note := siblingSyntaxNote(w.Root, abs, src); note != "" {
					out += "\n" + note
				}
				if reason, excluded := buildscope.Excluded(buildscope.Context(), abs); excluded {
					out += "\n" + excludedNote("This file", reason)
				}
			}
			return out, nil
		})

	add(s, h, "symbol_info", "What is X and where is it used? e.g. name=\"Square.Area\". Header, hover, definition and references grouped by file.",
		func(ctx context.Context, w *Workspace, in symInfoIn) (string, error) {
			name, err := params.ResolveName("Square.Area", params.Alias{Name: "name", Value: in.Name}, params.Alias{Name: "query", Value: in.Query})
			if err != nil {
				return "", err
			}
			sym, err := resolveSymbolRO(ctx, w, name, in.FilePath)
			if err != nil {
				return "", err
			}
			rel, line, col := sym.Rel(w.Root), sym.Line+1, sym.Column+1
			raw, err := w.Client.Hover(ctx, rel, line, col)
			if err != nil {
				return "", w.navErr(ctx, err, rel, line, col)
			}
			defs, err := w.Client.Definition(ctx, rel, line, col)
			if err != nil {
				return "", w.navErr(ctx, err, rel, line, col)
			}
			defs = dropGeneratedLocs(defs)
			ht := cgoHover(lsp.URIToPath(sym.URI), tidyHover(hoverText(raw)))
			if ht == "" {
				ht = "No hover information."
			}
			dt := "No definition found."
			if len(defs) > 0 {
				dt = format.FormatLocations(defs, w.Root)
			}
			parts := []string{fmt.Sprintf("%s  [%s]  (%s:%d:%d)", sym.Name, format.KindLabel(sym.Kind), rel, line, col), "", ht, "", "Definition:", dt}
			if in.IncludeReferences == nil || *in.IncludeReferences {
				refs, err := w.Client.References(ctx, rel, line, col, true)
				if err != nil {
					return "", w.navErr(ctx, err, rel, line, col)
				}
				refs = dropGeneratedLocs(refs)
				prod, tests := format.SplitTests(refs)
				parts = append(parts, "", "References:", format.FormatReferencesGrouped(prod, w.Root, format.DefaultReferenceFileLimit, false))
				if len(tests) > 0 {
					parts = append(parts, "", "In tests:", format.FormatReferencesGrouped(tests, w.Root, format.DefaultReferenceFileLimit, false))
				}
			}
			if note := uncoveredNote(w, sym); note != "" {
				parts = append(parts, "", note)
			}
			if note := caseNote(w, name, sym); note != "" {
				parts = append([]string{note, ""}, parts...)
			}
			return strings.Join(parts, "\n"), nil
		})

	add(s, h, "outline", "Indented outline (types, funcs, methods with line spans) of a Go file.",
		func(ctx context.Context, w *Workspace, in fileIn) (string, error) {
			if err := checkGoFile(w, in.FilePath); err != nil {
				return "", err
			}
			syms, err := w.Client.DocumentSymbol(ctx, in.FilePath)
			if err != nil {
				return "", err
			}
			ptrs := make([]*lsp.Symbol, len(syms))
			for i := range syms {
				ptrs[i] = &syms[i]
			}
			var groups []gosrc.Group
			abs := in.FilePath
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(w.Root, abs)
			}
			if text, _, err := edits.ReadSource(abs); err == nil {
				if pf, err := gosrc.ParseFile(abs, text); err == nil {
					groups = pf.Groups()
				}
			}
			return withNote(ownSyntaxNote(w, abs), format.FormatOutlineGroups(ptrs, groups)), nil
		})

	add(s, h, "callers", "Who calls this function or method? Only real call sites, unlike references.",
		func(ctx context.Context, w *Workspace, in nameIn) (string, error) {
			name, err := params.ResolveName("Scale", params.Alias{Name: "name", Value: in.Name}, params.Alias{Name: "query", Value: in.Query})
			if err != nil {
				return "", err
			}
			sym, err := resolveSymbolRO(ctx, w, name, in.FilePath)
			if err != nil {
				return "", err
			}
			items, err := w.Client.PrepareCallHierarchy(ctx, sym.Rel(w.Root), sym.Line+1, sym.Column+1)
			if err != nil {
				if strings.Contains(err.Error(), "is not a function") {
					return "", toolerr.Inputf("%s is %snot a function; callers needs a function or method.", sym.Name, kindPhrase(sym.Kind))
				}
				return "", err
			}
			if len(items) == 0 {
				return fmt.Sprintf("%s has no call hierarchy entry (it may not be callable).", sym.Name), nil
			}
			calls, err := w.Client.IncomingCalls(ctx, items[0])
			if err != nil {
				return "", err
			}
			calls, omitted := realCalls(dropGeneratedCalls(calls))
			out := format.FormatCallers(calls, w.Root, callerReceivers(calls))
			if omitted > 0 {
				out += fmt.Sprintf("\n(%d non-call reference(s) omitted: uses as a function or method value, such as f := %s; see references)", omitted, sym.Name)
			}
			if note := uncoveredNote(w, sym); note != "" {
				out += "\n" + note
			}
			if note := excludedUsesNote(w, sym); note != "" {
				out += "\n" + note
			}
			if inInterface(sym) {
				note := "Note: calls through in-workspace concrete implementations are included"
				if sc := nestedScan(w.Root); len(sc.mods) > 0 || sc.truncated {
					note += "; calls from nested modules the workspace does not cover are not"
				}
				out += "\n" + note + "."
			}
			if note := caseNote(w, name, sym); note != "" {
				out = note + "\n" + out
			}
			return out, nil
		})

	add(s, h, "implementations", "For an interface (or interface method): the types implementing it. For a concrete type (or method): the interfaces it satisfies.",
		func(ctx context.Context, w *Workspace, in nameIn) (string, error) {
			name, err := params.ResolveName("Shape", params.Alias{Name: "name", Value: in.Name}, params.Alias{Name: "query", Value: in.Query})
			if err != nil {
				return "", err
			}
			sym, err := resolveSymbolRO(ctx, w, name, in.FilePath)
			if err != nil {
				return "", err
			}
			locs, err := w.Client.Implementation(ctx, sym.Rel(w.Root), sym.Line+1, sym.Column+1)
			if err != nil {
				if strings.Contains(err.Error(), "is a function, not a method") {
					return "", toolerr.Inputf("%s is a function; implementations needs an interface, a type or an interface method.", sym.Name)
				}
				return "", err
			}
			locs = dropGeneratedLocs(dropForeignUnexported(locs, w.Root))
			out := fmt.Sprintf("No implementations or implemented interfaces found for %s.", sym.Name)
			if len(locs) > 0 {
				out = implementerNames(locs, w.Root) + splitTests(locs, w.Root)
			}
			out = withNote(nestedImplementersNote(w, name), out)
			if note := caseNote(w, name, sym); note != "" {
				out = note + "\n" + out
			}
			return out, nil
		})
}

// implementerNames lists the type behind each location, grouped (see implementerGroups). "" when no location is
// inside a type or a method.
func implementerNames(locs []lsp.Location, root string) string {
	return implementerGroups(locs, root, toolchainLocs(root))
}

// The classes of a location: the workspace's own files, the standard library, or a dependency.
const (
	clsWorkspace = iota
	clsStdlib
	clsDeps
)

// locClass classifies the file p of a workspace rooted at root. work are the directories of the go.work's other
// modules (their files are the workspace's); vendor files are dependencies even under root.
func locClass(root string, work []string, dirs toolchainDirs, p string) int {
	if under(filepath.Join(root, "vendor"), p) {
		return clsDeps
	}
	if under(root, p) {
		return clsWorkspace
	}
	for _, d := range work {
		if under(d, p) {
			return clsWorkspace
		}
	}
	if dirs.GOROOT != "" && under(dirs.GOROOT, p) {
		return clsStdlib
	}
	return clsDeps
}

// packageName is the package clause of the file p (its directory's name when the clause cannot be read).
func packageName(p, text string) string {
	if f, err := parser.ParseFile(token.NewFileSet(), p, text, parser.PackageClauseOnly); err == nil && f.Name != nil {
		return f.Name.Name
	}
	return filepath.Base(filepath.Dir(p))
}

// implementerGroups lists the types behind the locations in three groups, each on its own line: the workspace's own
// types as "Types (N)", then the standard library's and the dependencies' as "Standard library" and "Dependencies",
// qualified by their package name (io.Writer), so a name is not mistaken for a workspace type.
func implementerGroups(locs []lsp.Location, root string, dirs toolchainDirs) string {
	const shown = 20
	labels := [3]string{"Types", "Standard library", "Dependencies"}
	work := gomod.WorkModules(root)
	var names [3][]string
	seen := map[string]bool{}
	for _, l := range locs {
		p := lsp.URIToPath(l.URI)
		text, _, err := edits.ReadSource(p)
		if err != nil {
			continue
		}
		off := edits.Offset(text, l.Range.Start.Line, l.Range.Start.Character)
		name := enclosingTypeName(p, text, off)
		if name == "" {
			name = receiverTypeAt(p, text, off)
		}
		if name == "" {
			continue
		}
		g := locClass(root, work, dirs, p)
		if g != clsWorkspace {
			name = packageName(p, text) + "." + name
		}
		key := fmt.Sprintf("%d %s", g, name)
		if !seen[key] {
			seen[key] = true
			names[g] = append(names[g], name)
		}
	}
	var parts []string
	for g := range names {
		if len(names[g]) == 0 {
			continue
		}
		list, more := names[g], ""
		if len(list) > shown {
			more = fmt.Sprintf(", ... (+%d more)", len(list)-shown)
			list = list[:shown]
		}
		parts = append(parts, fmt.Sprintf("%s (%d): %s%s", labels[g], len(names[g]), strings.Join(list, ", "), more))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n") + "\n\n"
}

// The nested-module scan: at most nestedLimit modules, within nestedBudget, and the answer is kept for nestedTTL per
// root so that repeated implementations and callers calls do not walk the tree again.
const (
	nestedLimit  = 3
	nestedBudget = 2 * time.Second
	nestedTTL    = 30 * time.Second
)

// nestedScanResult lists the modules below the root that the workspace does not cover. truncated says the scan
// stopped at the limit or the time budget, so a module it did not reach may exist.
type nestedScanResult struct {
	mods      []gomod.Nested
	truncated bool
}

var nestedCache struct {
	sync.Mutex
	at  map[string]time.Time
	res map[string]nestedScanResult
}

// nestedScan returns the scan of root, from the cache while it is fresh.
func nestedScan(root string) nestedScanResult {
	nestedCache.Lock()
	defer nestedCache.Unlock()
	if t, ok := nestedCache.at[root]; ok && time.Since(t) < nestedTTL {
		return nestedCache.res[root]
	}
	res := scanNested(root, time.Now().Add(nestedBudget))
	if nestedCache.at == nil {
		nestedCache.at = map[string]time.Time{}
		nestedCache.res = map[string]nestedScanResult{}
	}
	nestedCache.at[root] = time.Now()
	nestedCache.res[root] = res
	return res
}

// scanNested walks root for go.mod files of modules the workspace does not cover, until the limit or the deadline.
func scanNested(root string, deadline time.Time) nestedScanResult {
	var res nestedScanResult
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if time.Now().After(deadline) {
			res.truncated = true
			return filepath.SkipAll
		}
		if d.IsDir() {
			if p != root && (exclude.IsDirName(d.Name()) || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" || filepath.Dir(p) == root {
			return nil
		}
		n, ok := gomod.Uncovered(root, filepath.Join(filepath.Dir(p), "x.go"))
		if !ok {
			return nil
		}
		if len(res.mods) == nestedLimit {
			res.truncated = true
			return filepath.SkipAll
		}
		res.mods = append(res.mods, n)
		return nil
	})
	return res
}

// nestedImplementersNote says when a module below the workspace that its go.mod/go.work does not cover mentions the
// queried name: gopls does not see its implementers, so an answer that lists none may be incomplete. A scan that
// stopped early is said so as well, since a module it did not reach may mention the name. "" otherwise.
func nestedImplementersNote(w *Workspace, query string) string {
	if strings.TrimSpace(query) == "" {
		return ""
	}
	scan := nestedScan(w.Root)
	var lines []string
	for _, n := range scan.mods {
		if !mayLiveIn(n, query) {
			continue
		}
		rel := edits.RelativeName(n.Dir, w.Root)
		lines = append(lines, fmt.Sprintf("Note: nested module %s (%s/go.mod) is not covered by the workspace's go.mod/go.work and mentions %s; its implementers and implemented interfaces are not listed here. A go.work listing the modules (go work init . ./%s) joins them.", n.Path, rel, query, rel))
	}
	if scan.truncated {
		lines = append(lines, fmt.Sprintf("Note: the scan for nested modules the workspace does not cover stopped early (after %d modules or %s); a module it did not reach may mention %s too.", nestedLimit, nestedBudget, query))
	}
	return strings.Join(lines, "\n")
}

// receiverTypeAt is the receiver type of the method declared around offset off ("" if none).
func receiverTypeAt(filename, src string, off int) string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return ""
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 || off < fset.Position(fd.Pos()).Offset || off >= fset.Position(fd.End()).Offset {
			continue
		}
		t := fd.Recv.List[0].Type
		if st, ok := t.(*ast.StarExpr); ok {
			t = st.X
		}
		if ix, ok := t.(*ast.IndexExpr); ok {
			t = ix.X
		}
		if id, ok := t.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// dropForeignUnexported removes types declared outside the workspace under an unexported name (the stdlib's
// `stringer` copies in context or runtime, `pp` in fmt): nobody can name them. A location on a method counts as
// its receiver's type. It keeps everything when nothing else would remain.
func dropForeignUnexported(locs []lsp.Location, root string) []lsp.Location {
	work := gomod.WorkModules(root)
	var kept []lsp.Location
	for _, l := range locs {
		p := lsp.URIToPath(l.URI)
		if locClass(root, work, toolchainDirs{}, p) == clsWorkspace {
			kept = append(kept, l)
			continue
		}
		text, _, err := edits.ReadSource(p)
		if err != nil {
			kept = append(kept, l)
			continue
		}
		off := edits.Offset(text, l.Range.Start.Line, l.Range.Start.Character)
		name := enclosingTypeName(p, text, off)
		if name == "" {
			name = receiverTypeAt(p, text, off)
		}
		if name != "" {
			if r, _ := utf8.DecodeRuneInString(name); unicode.IsLower(r) || r == '_' {
				continue
			}
		}
		kept = append(kept, l)
	}
	if len(kept) == 0 {
		return locs
	}
	return kept
}

// enclosingTypeName is the name of the top-level type declaration containing byte offset off ("" if none).
func enclosingTypeName(filename, src string, off int) string {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return ""
	}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, sp := range g.Specs {
			if ts, ok := sp.(*ast.TypeSpec); ok && fset.Position(ts.Pos()).Offset <= off && off < fset.Position(ts.End()).Offset {
				return ts.Name.Name
			}
		}
	}
	return ""
}

// kindPhrase describes an LSP symbol kind for "X is a type, not a function".
func kindPhrase(kind int) string {
	switch kind {
	case 5, 11, 23, 26: // class, interface, struct, type parameter
		return "a type, "
	case 13:
		return "a variable, "
	case 14:
		return "a constant, "
	case 8:
		return "a field, "
	}
	return ""
}

func splitTests(locs []lsp.Location, root string) string {
	prod, tests := format.SplitTests(locs)
	out := format.FormatReferences(prod, root, format.DefaultReferencesSnippetLimit)
	if len(prod) == 0 {
		out = ""
	}
	if len(tests) > 0 {
		if out != "" {
			out += "\n\n"
		}
		out += "In tests:\n" + format.FormatReferences(tests, root, format.DefaultReferencesSnippetLimit)
	}
	if out == "" {
		return "No references found at that position."
	}
	return out
}

// realCalls drops the sites gopls' incoming calls include that are not calls: a function or
// method value (f := Target, var h func() int = Target, m := t.M). A site is kept when its
// identifier is the callee of a call expression. Files that cannot be read or parsed are kept as is.
func realCalls(calls []lsp.IncomingCall) (kept []lsp.IncomingCall, omitted int) {
	parsed := map[string]*gosrc.File{}
	for _, c := range calls {
		p := lsp.URIToPath(c.From.URI)
		pf, seen := parsed[p]
		if !seen {
			if text, _, err := edits.ReadSource(p); err == nil {
				pf, _ = gosrc.ParseFile(p, text)
			}
			parsed[p] = pf
		}
		if pf == nil {
			kept = append(kept, c)
			continue
		}
		var ranges []lsp.Range
		for _, r := range c.FromRanges {
			if pf.CallAt(edits.Offset(pf.Src, r.Start.Line, r.Start.Character)) != nil {
				ranges = append(ranges, r)
			} else {
				omitted++
			}
		}
		if len(ranges) > 0 || len(c.FromRanges) == 0 {
			c.FromRanges = ranges
			kept = append(kept, c)
		}
	}
	return kept, omitted
}

// callerReceivers finds, per call, the receiver type of the calling method: gopls' call hierarchy
// reports methods as plain functions. Each caller file is parsed once.
func callerReceivers(calls []lsp.IncomingCall) map[int]string {
	parsed := map[string]*gosrc.File{}
	recvs := map[int]string{}
	for i, c := range calls {
		p := lsp.URIToPath(c.From.URI)
		pf, seen := parsed[p]
		if !seen {
			if text, _, err := edits.ReadSource(p); err == nil {
				pf, _ = gosrc.ParseFile(p, text)
			}
			parsed[p] = pf
		}
		if pf == nil {
			continue
		}
		start := c.From.SelectionRange.Start
		if recv, ok := pf.FuncAt(edits.Offset(pf.Src, start.Line, start.Character)); ok && recv != "" {
			recvs[i] = recv
		}
	}
	return recvs
}

// shortQuery cuts a long query for echoing it back.
func shortQuery(q string) string {
	const max = 80
	if r := []rune(q); len(r) > max {
		return string(r[:max]) + "..."
	}
	return q
}

// workspaceReport is the workspace tool's answer: the root, why it was chosen, and the settings that decide what
// the other answers mean (read-only mode, build flags, gopls, the Go toolchain and the go.work in use). The go
// command is asked in the root, so GOWORK and GOFLAGS are the values the server runs with.
func workspaceReport(ctx context.Context, h Host, w *Workspace) string {
	lines := []string{w.Root, "chosen because: " + h.Explain(w.Source)}
	readOnly := "no"
	if (w.Journal != nil && w.Journal.Guard.ReadOnly) || (w.Journal == nil && envTrue(ReadOnlyEnv)) {
		readOnly = "yes (" + ReadOnlyEnv + ")"
	}
	lines = append(lines, "read-only: "+readOnly)
	lines = append(lines, "build flags (CODENAV_GO_MCP_BUILD_FLAGS): "+orNone(redactFlags(envOr("CODENAV_GO_MCP_BUILD_FLAGS", ""))))
	lines = append(lines, "gopls: "+goplsDescribe(ctx))
	vals, err := goEnv(ctx, w.Root, "GOVERSION", "GOFLAGS", "GOWORK")
	if err != nil {
		vals = []string{"unknown", "unknown", "unknown"}
	}
	lines = append(lines, "go: "+orNone(vals[0]), "GOFLAGS: "+orNone(redactFlags(vals[1])))
	lines = append(lines, "GOWORK env: "+envOr("GOWORK", "unset"), "go.work in use: "+orNone(vals[2]))
	return strings.Join(lines, "\n")
}

// envTrue reports whether the environment variable is set to a true-like value (as the server reads its flags).
func envTrue(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// envOr is the variable's value, or fallback when it is unset or blank.
func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// orNone is s, or "none" when it is empty.
func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// redactFlags prints a flag string with the values of -ldflags and -X hidden: those carry -X name=value pairs,
// which are often secrets. The flag names stay, so the build is still described; the other flags are printed as
// they are.
func redactFlags(s string) string {
	var out []string
	eliding := false
	for _, f := range strings.Fields(s) {
		if !strings.HasPrefix(f, "-") {
			if !eliding {
				out = append(out, f)
			}
			continue
		}
		name, _, _ := strings.Cut(f, "=")
		switch strings.TrimLeft(name, "-") {
		case "ldflags", "X":
			out = append(out, name+"=<redacted>")
			eliding = true
		default:
			out = append(out, f)
			eliding = false
		}
	}
	return strings.Join(out, " ")
}

// goEnv runs `go env` for names in dir and returns one trimmed value per name (empty when unset). It is an error
// when the go command cannot run.
func goEnv(ctx context.Context, dir string, names ...string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", append([]string{"env"}, names...)...)
	cmd.Dir = dir
	b, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	vals := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	out := make([]string, len(names))
	for i := range names {
		if i < len(vals) {
			out[i] = strings.TrimSpace(vals[i])
		}
	}
	return out, nil
}

// goplsDescribe names the gopls the server runs, with its version. It runs the same command the server starts, with
// `version` after its arguments. An unusable pin or a missing gopls is said as such; a go run fallback is named, not
// run, since running it would fetch gopls.
func goplsDescribe(ctx context.Context) string {
	cmd, warn, err := goplscmd.Command()
	if err != nil {
		return "not usable: " + err.Error()
	}
	shown := strings.Join(cmd, " ")
	if warn != "" {
		return shown + " (fallback: gopls is not installed; its version is not asked, since go run would fetch it)"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := append(append([]string{}, cmd[1:]...), "version")
	out, err := exec.CommandContext(ctx, cmd[0], args...).Output()
	version := "version unknown"
	if err != nil {
		version = "version failed: " + err.Error()
	} else if first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n"); first != "" {
		version = first
	}
	return fmt.Sprintf("%s (%s)", shown, version)
}

// toolchainDirs are the Go root and the module cache: the places whose files are dependencies of the workspace.
type toolchainDirs struct{ GOROOT, GOMODCACHE string }

var toolchain struct {
	sync.Mutex
	byRoot map[string]toolchainDirs
}

// toolchainLocs returns the Go root and module cache as the go command run in root reports them. A successful
// answer is kept per root; a failed `go env` is not kept, so the next call asks again. Empty values when unknown.
func toolchainLocs(root string) toolchainDirs {
	toolchain.Lock()
	defer toolchain.Unlock()
	if d, ok := toolchain.byRoot[root]; ok {
		return d
	}
	vals, err := goEnv(context.Background(), root, "GOROOT", "GOMODCACHE")
	if err != nil {
		return toolchainDirs{}
	}
	d := toolchainDirs{GOROOT: vals[0], GOMODCACHE: vals[1]}
	if d.GOROOT != "" && d.GOMODCACHE != "" {
		if toolchain.byRoot == nil {
			toolchain.byRoot = map[string]toolchainDirs{}
		}
		toolchain.byRoot[root] = d
	}
	return d
}

// plural is "s" for every count but one.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// inInterface reports whether the symbol is declared inside an interface type (an interface method).
func inInterface(sym resolve.Symbol) bool {
	path := lsp.URIToPath(sym.URI)
	text, _, err := edits.ReadSource(path)
	if err != nil {
		return false
	}
	return interfaceTypeAt(path, text, edits.Offset(text, sym.Line, sym.Column))
}

// interfaceTypeAt reports whether byte offset off of filename lies in a type declaration whose type is an interface.
func interfaceTypeAt(filename, src string, off int) bool {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return false
	}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, sp := range g.Specs {
			ts, ok := sp.(*ast.TypeSpec)
			if !ok || off < fset.Position(ts.Pos()).Offset || off >= fset.Position(ts.End()).Offset {
				continue
			}
			_, isIface := ts.Type.(*ast.InterfaceType)
			return isIface
		}
	}
	return false
}

// dropDependencyLocs removes the locations of dependencies: the Go root, the module cache and the vendor directory.
// A workspace module's own file is kept even when it sits under one of those directories. It returns the rest and
// how many it dropped.
func dropDependencyLocs(locs []lsp.Location, root string, dirs toolchainDirs) ([]lsp.Location, int) {
	work := gomod.WorkModules(root)
	var kept []lsp.Location
	dropped := 0
	for _, l := range locs {
		p := lsp.URIToPath(l.URI)
		c := locClass(root, work, dirs, p)
		inModcache := dirs.GOMODCACHE != "" && under(dirs.GOMODCACHE, p)
		inVendor := under(filepath.Join(root, "vendor"), p)
		if c == clsStdlib || (c == clsDeps && (inModcache || inVendor)) {
			dropped++
			continue
		}
		kept = append(kept, l)
	}
	return kept, dropped
}

// referencesAnswer is the references answer for locs: the workspace's references and the count of dependency call
// sites left out. When every reference is a dependency's, the answer says that instead of "No references found".
func referencesAnswer(locs []lsp.Location, root string, dirs toolchainDirs) string {
	kept, deps := dropDependencyLocs(locs, root, dirs)
	switch {
	case deps == 0:
		return splitTests(kept, root)
	case len(kept) == 0:
		return fmt.Sprintf("%d reference(s) inside dependencies not shown (references lists the workspace only).", deps)
	}
	return splitTests(kept, root) + fmt.Sprintf("\n(%d reference(s) inside dependencies not shown)", deps)
}
