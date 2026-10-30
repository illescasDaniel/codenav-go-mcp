package tools

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/resolve"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

// resolveSymbol is resolve.Resolve with a Go-aware explanation when nothing matches.
func resolveSymbol(ctx context.Context, w *Workspace, name, file string) (resolve.Symbol, error) {
	sym, err := resolve.Resolve(ctx, w.Client, w.Root, name, file)
	var nf *resolve.NotFoundError
	if errors.As(err, &nf) {
		return sym, toolerr.Inputf("%s", notFoundMessage(ctx, w, nf.Query, false))
	}
	return sym, err
}

// resolveSymbolRO is resolveSymbol for the read-only tools: a package-qualified name that is not in the
// workspace (json.Marshal, encoding/json.Decoder.Decode, a dependency's Client.Do) is looked up through
// a scratch gopls overlay (resolve.Probe). Write tools keep to workspace symbols: their guard refuses
// GOROOT and the module cache anyway.
func resolveSymbolRO(ctx context.Context, w *Workspace, name, file string) (resolve.Symbol, error) {
	sym, err := resolve.Resolve(ctx, w.Client, w.Root, name, file)
	var nf *resolve.NotFoundError
	if !errors.As(err, &nf) {
		return sym, err
	}
	psym, perr := resolve.Probe(ctx, w.Client, w.Root, nf.Query, workspaceImports(w.Root, file))
	switch {
	case perr == nil:
		return psym, nil
	case errors.Is(perr, resolve.ErrNoProbe):
		return sym, toolerr.Inputf("%s", notFoundMessage(ctx, w, nf.Query, false))
	}
	var pe *resolve.ProbeError
	if errors.As(perr, &pe) {
		return sym, toolerr.Inputf("No symbol found matching %q: %s.", nf.Query, pe.Msg)
	}
	return sym, perr
}

// maxImportScan bounds how many workspace files workspaceImports parses.
const maxImportScan = 3000

// workspaceImports maps each qualifier the workspace's Go files import packages under to the import
// paths (the most used first; file's own imports lead).
func workspaceImports(root, file string) map[string][]string {
	count := map[string]map[string]int{}
	note := func(path string, weight int) {
		src, _, err := edits.ReadSource(path)
		if err != nil {
			return
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
		if err != nil {
			return
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			name := gosrc.ImportName(p)
			if imp.Name != nil {
				if imp.Name.Name == "_" || imp.Name.Name == "." {
					continue
				}
				name = imp.Name.Name
			}
			if count[name] == nil {
				count[name] = map[string]int{}
			}
			count[name][p] += weight
		}
	}
	if file != "" {
		p := file
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		note(p, 1_000_000)
	}
	n := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return nil
		case d.IsDir():
			if p != root && (exclude.IsDirName(d.Name()) || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
		case strings.HasSuffix(p, ".go"):
			if n++; n > maxImportScan {
				return filepath.SkipAll
			}
			note(p, 1)
		}
		return nil
	})
	out := map[string][]string{}
	for name, paths := range count {
		for p := range paths {
			out[name] = append(out[name], p)
		}
		sort.Slice(out[name], func(i, j int) bool {
			a, b := out[name][i], out[name][j]
			return paths[a] > paths[b] || paths[a] == paths[b] && a < b
		})
	}
	return out
}

// builtins are Go's predeclared identifiers: they have no declaration to find.
var builtins = map[string]bool{
	"append": true, "cap": true, "clear": true, "close": true, "complex": true, "copy": true, "delete": true,
	"imag": true, "len": true, "make": true, "max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true, "any": true, "bool": true, "byte": true, "comparable": true,
	"complex64": true, "complex128": true, "error": true, "float32": true, "float64": true, "int": true, "int8": true,
	"int16": true, "int32": true, "int64": true, "rune": true, "string": true, "uint": true, "uint8": true,
	"uint16": true, "uint32": true, "uint64": true, "uintptr": true, "true": true, "false": true, "iota": true, "nil": true,
}

// typeKinds are the LSP symbol kinds gopls uses for type declarations.
var typeKinds = map[int]bool{5: true, 10: true, 11: true, 23: true, 26: true}

// notFoundMessage explains why query matches no workspace symbol, with the hint that fits.
//
// ro is set for the read-only tools, which already tried a package-qualified name outside the workspace.
func notFoundMessage(ctx context.Context, w *Workspace, query string, ro bool) string {
	msg := fmt.Sprintf("No symbol found matching %q", query)
	parts := strings.Split(query, ".")
	first, last := parts[0], parts[len(parts)-1]
	outside := "; write tools resolve only workspace symbols - symbol_info, callers and implementations also take json.Marshal or encoding/json.Decoder.Decode, and hover or definition work at a use of any symbol."
	if ro {
		outside = "; for a standard-library or dependency symbol spell the import path (math/rand.Int, github.com/x/y.Client.Do; the module must be in go.mod)."
	}
	switch {
	case len(parts) == 1 && builtins[query]:
		msg += ": " + query + " is a Go builtin (predeclared), with no declaration to show."
	case len(parts) == 1 && packageDir(w.Root, query) != "":
		dir := packageDir(w.Root, query)
		msg += fmt.Sprintf(": %s is a package (directory %s), not a symbol; outline its files or use search_symbol with path=%s.", query, dir, dir)
	case len(parts) == 1:
		msg += " (struct fields and methods are named Type.Name)."
	case isWorkspaceType(ctx, w, first):
		msg += fmt.Sprintf(": type %s declares no %s (methods promoted through embedding are not declared symbols; query the embedded type's method instead).", first, last)
	case packageDir(w.Root, first) != "":
		msg += fmt.Sprintf(": package %s declares no %s.", first, strings.Join(parts[1:], "."))
	case isStdlib(first):
		msg += fmt.Sprintf(": %s is in the standard library, outside the workspace%s", first, outside)
	default:
		msg += fmt.Sprintf(": no type or package named %s in the workspace%s", first, outside)
	}
	if nested := gomod.UncoveredModules(w.Root, exclude.IsDirName, 3); len(nested) > 0 {
		var names []string
		for _, n := range nested {
			rel := edits.RelativeName(n.Dir, w.Root)
			if n.Path != "" {
				rel = n.Path + " (" + rel + ")"
			}
			names = append(names, rel)
		}
		msg += fmt.Sprintf("\nNote: module(s) %s sit below the workspace without a go.work listing them; gopls indexes each as a separate folder, so other modules importing them are not seen. A go.work (go work init . ./<dir>) joins them.", strings.Join(names, ", "))
	}
	return msg
}

func isStdlib(qualifier string) bool {
	spec, ambiguous := gosrc.StdlibImport(qualifier)
	return spec != "" || ambiguous
}

// isWorkspaceType reports whether a type called name is declared in the workspace.
func isWorkspaceType(ctx context.Context, w *Workspace, name string) bool {
	syms, err := w.Client.WorkspaceSymbol(ctx, name)
	if err != nil {
		return false
	}
	for _, s := range syms {
		if s.Name == name && typeKinds[s.Kind] && s.Location != nil && w.inWorkspace(filepath.Clean(lsp.URIToPath(s.Location.URI))) {
			return true
		}
	}
	return false
}
