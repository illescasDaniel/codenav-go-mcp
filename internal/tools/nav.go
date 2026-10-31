// Package tools holds the MCP tool handlers.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/format"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
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

// add registers a read-only text tool; expected errors become tool text.
func add[In any](s *mcp.Server, h Host, name, desc string, fn func(ctx context.Context, w *Workspace, in In) (string, error)) {
	registerUsage[In](name)
	t := &mcp.Tool{Name: name, Description: desc, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		w, err := h.Acquire(ctx, req.Session)
		var out string
		if err == nil {
			if err = w.checkReadable(filePathOf(in)); err == nil {
				out, err = fn(ctx, w, in)
			}
		}
		if err != nil {
			t, ok := toolerr.Format(err)
			if !ok {
				return nil, nil, err
			}
			out = t
			if w != nil {
				out = w.scrub(out)
			}
		}
		return text(h.Notes().Annotate(w.withConfigProblem(ctx, out))), nil, nil
	})
}

// withConfigProblem appends the go command's complaint about a broken go.mod / go.work, which otherwise
// shows up as unrelated "nothing found" answers (gopls loads no packages at all).
func (w *Workspace) withConfigProblem(ctx context.Context, out string) string {
	if w == nil {
		return out
	}
	if p := gomod.ConfigProblem(ctx, w.Root); p != "" {
		return out + "\n\nWarning: the Go configuration of this workspace is broken, so no package is loaded and type-resolved results (symbols, diagnostics, rename) are empty or wrong: " + p
	}
	return out
}

// addIndexed adds the exact and prefix matches of the symbol index to gopls's (capped, build-config
// bound) workspace symbols, so those cannot be missing; gopls's fuzzy results stay for the rest. A
// declaration gopls already reported (same file and line) is not added twice.
func addIndexed(w *Workspace, q string, have []*lsp.Symbol) []*lsp.Symbol {
	if w.Index == nil {
		return have
	}
	type key struct {
		uri  string
		line int
	}
	seen := map[key]bool{}
	for _, s := range have {
		if uri, l, _ := format.SymbolPosition(s); uri != "" {
			seen[key{uri, l}] = true
		}
	}
	for _, e := range w.Index.Prefix(q) {
		s := resolve.FromEntry(e)
		if k := (key{s.Location.URI, e.Line}); !seen[k] {
			seen[k] = true
			have = append(have, s)
		}
	}
	return have
}

var (
	goCacheOnce sync.Once
	goCacheDir  string
)

// goCache is the go build cache directory (`go env GOCACHE`), "" when unknown.
func goCache() string {
	goCacheOnce.Do(func() {
		if out, err := exec.Command("go", "env", "GOCACHE").Output(); err == nil {
			goCacheDir = strings.TrimSpace(string(out))
		}
	})
	return goCacheDir
}

// isCgoGenerated: a file cgo wrote into the go build cache.
func isCgoGenerated(p string) bool {
	cache := goCache()
	return cache != "" && strings.HasPrefix(p, cache+string(filepath.Separator)) || strings.Contains(filepath.ToSlash(p), "/go-build/")
}

var cgoNameRE = regexp.MustCompile(`\b_C(?:func|type|var|const|macro)_(\w+)`)

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

// generatedPrefixes start the names cgo generates (`_Cfunc_add`, `_cgo_...`).
var generatedPrefixes = []string{"_Cfunc_", "_Ctype_", "_Cvar_", "_Cmacro_", "_Cconst_", "_cgo_", "__cgofn__"}

// dropNoise removes symbols from files the go tool never compiles (`_x.go`, `//go:build ignore`) and
// the generated ones cgo leaves in the build cache.
func dropNoise(syms []*lsp.Symbol) []*lsp.Symbol {
	cache := goCache()
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
		generated := false
		for _, pre := range generatedPrefixes {
			if strings.HasPrefix(s.Name, pre) {
				generated = true
			}
		}
		if !generated {
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

func checkGoFile(p string) error {
	if !strings.HasSuffix(p, ".go") {
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
	Path  string `json:"path,omitempty" jsonschema:"workspace-relative prefix or glob"`
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

// A replaced module (directory) has no version, so gopls prints `pkg.go.dev/../dir@#Name`; those go too.
// So do links to a dotted import path without a version (example.com/x#Name): that is a module of the
// workspace itself (the main module, a go.work "use", a local replace), which pkg.go.dev does not know.
// The stdlib (no dot in the first element) and versioned module-cache dependencies keep theirs.
var adHocLink = regexp.MustCompile(`(?m)\n*(?:---\n+)?\[[^\]\n]*\]\(https://pkg\.go\.dev/(?:command-line-arguments[^)\n]*|[./][^)\n]*|[^)\s]*@[#/][^)\n]*|[^)\s]*@|[^/)\s@]*\.[^/)\s@]*(?:/[^)\s@]*)?)\)[ \t]*`)

// tidyHover drops useless links and caps the length, keeping the head (signature and summary).
func tidyHover(t string) string {
	t = strings.TrimRight(adHocLink.ReplaceAllString(t, ""), "\n ")
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
		return toolerr.Inputf("No identifier at %s:%d:%d.", file, line, col)
	}
	return err
}

// RegisterNav registers the navigation tools.
func RegisterNav(s *mcp.Server, h Host) {
	add(s, h, "workspace", "Which directory is codenav navigating, and why? Use when results look like they come from the wrong checkout/worktree.",
		func(_ context.Context, w *Workspace, _ struct{}) (string, error) {
			return fmt.Sprintf("%s\nchosen because: %s", w.Root, h.Explain(w.Source)), nil
		})

	add(s, h, "hover", "Type/documentation info for the symbol at a position. line/column are 1-indexed; column is a UTF-16 offset.",
		func(ctx context.Context, w *Workspace, in posIn) (string, error) {
			if err := checkGoFile(in.FilePath); err != nil {
				return "", err
			}
			raw, err := w.Client.Hover(ctx, in.FilePath, in.Line, in.Column)
			if err != nil {
				return "", friendlyNavErr(err, in.FilePath, in.Line, in.Column)
			}
			if t := tidyHover(hoverText(raw)); t != "" {
				abs := in.FilePath
				if !filepath.IsAbs(abs) {
					abs = filepath.Join(w.Root, abs)
				}
				return cgoHover(abs, t), nil
			}
			return "No hover information at that position.", nil
		})

	add(s, h, "definition", "Go to the definition of the symbol at a position (type-resolved).",
		func(ctx context.Context, w *Workspace, in posIn) (string, error) {
			if err := checkGoFile(in.FilePath); err != nil {
				return "", err
			}
			if msg := cgoDefinition(w, in); msg != "" {
				return msg, nil
			}
			locs, err := w.Client.Definition(ctx, in.FilePath, in.Line, in.Column)
			if err != nil {
				return "", friendlyNavErr(err, in.FilePath, in.Line, in.Column)
			}
			if len(locs) == 0 {
				return "No definition found at that position.", nil
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
			return out, nil
		})

	add(s, h, "references", "All usages of the symbol at a position across the workspace. Hits in _test.go files are listed separately.",
		func(ctx context.Context, w *Workspace, in refsIn) (string, error) {
			if err := checkGoFile(in.FilePath); err != nil {
				return "", err
			}
			decl := in.IncludeDeclaration == nil || *in.IncludeDeclaration
			locs, err := w.Client.References(ctx, in.FilePath, in.Line, in.Column, decl)
			if err != nil {
				return "", friendlyNavErr(err, in.FilePath, in.Line, in.Column)
			}
			return splitTests(locs, w.Root), nil
		})

	add(s, h, "search_symbol", "Search the workspace for a symbol by name. Returns positions to feed to definition/references/hover. Production code ranks before tests.",
		func(ctx context.Context, w *Workspace, in searchIn) (string, error) {
			kinds, err := format.ParseKindFilter(in.Kind)
			if err != nil {
				return "", err
			}
			q, err := params.ResolveName("CreateUser", params.Alias{Name: "query", Value: in.Query}, params.Alias{Name: "name", Value: in.Name})
			if err != nil {
				return "", err
			}
			syms, err := w.Client.WorkspaceSymbol(ctx, q)
			if err != nil {
				return "", err
			}
			ptrs := make([]*lsp.Symbol, len(syms))
			for i := range syms {
				ptrs[i] = &syms[i]
			}
			ptrs = dropNoise(addIndexed(w, q, ptrs))
			if len(ptrs) == 0 {
				return fmt.Sprintf("No symbols matching %q.", shortQuery(q)), nil
			}
			kept := format.FilterByKindAndPath(ptrs, w.Root, kinds, in.Path)
			if len(kept) == 0 {
				return fmt.Sprintf("No symbols matching %q with the given kind/path filters (%d without them).", shortQuery(q), len(ptrs)), nil
			}
			return format.FormatWorkspaceSymbols(kept, w.Root, q, format.DefaultSearchSymbolLimit, in.Fuzzy), nil
		})

	add(s, h, "diagnostics", "gopls diagnostics (errors/warnings) for a single file.",
		func(ctx context.Context, w *Workspace, in fileIn) (string, error) {
			if err := checkGoFile(in.FilePath); err != nil {
				return "", err
			}
			abs := in.FilePath
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(w.Root, abs)
			}
			if rel, err := filepath.Rel(w.Root, abs); err != nil || strings.HasPrefix(rel, "..") {
				return "", toolerr.Inputf("%s is outside the workspace (%s): diagnostics only cover files of the workspace's own modules. Files of the standard library, the module cache and replaced modules elsewhere can be read (outline, hover, references) but are not type-checked as part of this workspace.", in.FilePath, w.Root)
			}
			items, err := w.Client.Diagnostics(ctx, in.FilePath)
			if err != nil {
				return "", friendlyNavErr(err, in.FilePath, 0, 0)
			}
			out := format.FormatDiagnostics(items, format.DefaultDiagnosticsLimit)
			if src, _, rerr := edits.ReadSource(abs); rerr == nil && gomod.HasBuildConstraint(src) {
				out += "\n(build-constrained: this file has a build constraint, and diagnostics only cover the current GOOS/GOARCH/tags; if the constraint excludes it here, it was not type-checked.)"
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
				return "", friendlyNavErr(err, rel, line, col)
			}
			defs, err := w.Client.Definition(ctx, rel, line, col)
			if err != nil {
				return "", friendlyNavErr(err, rel, line, col)
			}
			ht := tidyHover(hoverText(raw))
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
					return "", friendlyNavErr(err, rel, line, col)
				}
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
			if err := checkGoFile(in.FilePath); err != nil {
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
			return format.FormatOutlineGroups(ptrs, groups), nil
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
			calls, omitted := realCalls(calls)
			out := format.FormatCallers(calls, w.Root, callerReceivers(calls))
			if omitted > 0 {
				out += fmt.Sprintf("\n(%d non-call reference(s) omitted: uses as a function or method value, such as f := %s; see references)", omitted, sym.Name)
			}
			if note := uncoveredNote(w, sym); note != "" {
				out += "\n" + note
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
			if len(locs) == 0 {
				return fmt.Sprintf("No implementations or implemented interfaces found for %s.", sym.Name), nil
			}
			locs = dropForeignUnexported(locs, w.Root)
			out := implementerNames(locs) + splitTests(locs, w.Root)
			if note := caseNote(w, name, sym); note != "" {
				out = note + "\n" + out
			}
			return out, nil
		})
}

// implementerNames lists the type behind each location ("Implementers: A, B, ..."), so the answer
// names them before the source excerpts. "" when no location is inside a type or a method.
func implementerNames(locs []lsp.Location) string {
	const shown = 20
	var names []string
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
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	more := ""
	if len(names) > shown {
		more = fmt.Sprintf(", ... (+%d more)", len(names)-shown)
		names = names[:shown]
	}
	return fmt.Sprintf("Types (%d): %s%s\n\n", len(seen), strings.Join(names, ", "), more)
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

// dropForeignUnexported removes interfaces declared outside the workspace under an unexported name
// (the stdlib's `stringer` copies in context or runtime): nobody can name them. It keeps everything
// when nothing else would remain.
func dropForeignUnexported(locs []lsp.Location, root string) []lsp.Location {
	var kept []lsp.Location
	for _, l := range locs {
		p := lsp.URIToPath(l.URI)
		if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
			kept = append(kept, l)
			continue
		}
		text, _, err := edits.ReadSource(p)
		if err != nil {
			kept = append(kept, l)
			continue
		}
		if name := enclosingTypeName(p, text, edits.Offset(text, l.Range.Start.Line, l.Range.Start.Character)); name != "" {
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
