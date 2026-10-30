// Package tools holds the MCP tool handlers.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/format"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
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
	t := &mcp.Tool{Name: name, Description: desc, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		w, err := h.Acquire(ctx, req.Session)
		var out string
		if err == nil {
			out, err = fn(ctx, w, in)
		}
		if err != nil {
			t, ok := toolerr.Format(err)
			if !ok {
				return nil, nil, err
			}
			out = t
		}
		return text(h.Notes().Annotate(out)), nil, nil
	})
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
				return "", err
			}
			if t := hoverText(raw); t != "" {
				return t, nil
			}
			return "No hover information at that position.", nil
		})

	add(s, h, "definition", "Go to the definition of the symbol at a position (type-resolved).",
		func(ctx context.Context, w *Workspace, in posIn) (string, error) {
			if err := checkGoFile(in.FilePath); err != nil {
				return "", err
			}
			locs, err := w.Client.Definition(ctx, in.FilePath, in.Line, in.Column)
			if err != nil {
				return "", err
			}
			if len(locs) == 0 {
				return "No definition found at that position.", nil
			}
			return format.FormatLocations(locs, w.Root), nil
		})

	add(s, h, "references", "All usages of the symbol at a position across the workspace. Hits in _test.go files are listed separately.",
		func(ctx context.Context, w *Workspace, in refsIn) (string, error) {
			if err := checkGoFile(in.FilePath); err != nil {
				return "", err
			}
			decl := in.IncludeDeclaration == nil || *in.IncludeDeclaration
			locs, err := w.Client.References(ctx, in.FilePath, in.Line, in.Column, decl)
			if err != nil {
				return "", err
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
			if len(syms) == 0 {
				return fmt.Sprintf("No symbols matching %q.", q), nil
			}
			ptrs := make([]*lsp.Symbol, len(syms))
			for i := range syms {
				ptrs[i] = &syms[i]
			}
			kept := format.FilterByKindAndPath(ptrs, w.Root, kinds, in.Path)
			if len(kept) == 0 {
				return fmt.Sprintf("No symbols matching %q with the given kind/path filters (%d without them).", q, len(syms)), nil
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
				return "", toolerr.Inputf("%s is outside the workspace (%s).", in.FilePath, w.Root)
			}
			items, err := w.Client.Diagnostics(ctx, in.FilePath)
			if err != nil {
				return "", err
			}
			return format.FormatDiagnostics(items, format.DefaultDiagnosticsLimit), nil
		})

	add(s, h, "symbol_info", "What is X and where is it used? e.g. name=\"Square.Area\". Header, hover, definition and references grouped by file.",
		func(ctx context.Context, w *Workspace, in symInfoIn) (string, error) {
			name, err := params.ResolveName("Square.Area", params.Alias{Name: "name", Value: in.Name}, params.Alias{Name: "query", Value: in.Query})
			if err != nil {
				return "", err
			}
			sym, err := resolve.Resolve(ctx, w.Client, w.Root, name, in.FilePath)
			if err != nil {
				return "", err
			}
			rel, line, col := sym.Rel(w.Root), sym.Line+1, sym.Column+1
			raw, err := w.Client.Hover(ctx, rel, line, col)
			if err != nil {
				return "", err
			}
			defs, err := w.Client.Definition(ctx, rel, line, col)
			if err != nil {
				return "", err
			}
			ht := hoverText(raw)
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
					return "", err
				}
				prod, tests := format.SplitTests(refs)
				parts = append(parts, "", "References:", format.FormatReferencesGrouped(prod, w.Root, format.DefaultReferenceFileLimit, false))
				if len(tests) > 0 {
					parts = append(parts, "", "In tests:", format.FormatReferencesGrouped(tests, w.Root, format.DefaultReferenceFileLimit, false))
				}
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
			sym, err := resolve.Resolve(ctx, w.Client, w.Root, name, in.FilePath)
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
			return format.FormatCallers(calls, w.Root, callerReceivers(calls)), nil
		})

	add(s, h, "implementations", "For an interface (or interface method): the types implementing it. For a concrete type (or method): the interfaces it satisfies.",
		func(ctx context.Context, w *Workspace, in nameIn) (string, error) {
			name, err := params.ResolveName("Shape", params.Alias{Name: "name", Value: in.Name}, params.Alias{Name: "query", Value: in.Query})
			if err != nil {
				return "", err
			}
			sym, err := resolve.Resolve(ctx, w.Client, w.Root, name, in.FilePath)
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
			return splitTests(dropForeignUnexported(locs, w.Root), w.Root), nil
		})
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
