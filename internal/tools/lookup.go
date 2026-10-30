package tools

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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
		return sym, toolerr.Inputf("%s", notFoundMessage(ctx, w, nf.Query))
	}
	return sym, err
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
func notFoundMessage(ctx context.Context, w *Workspace, query string) string {
	msg := fmt.Sprintf("No symbol found matching %q", query)
	parts := strings.Split(query, ".")
	first, last := parts[0], parts[len(parts)-1]
	outside := "; name lookups cover only the workspace's own symbols - use hover or definition with file_path/line/column at a use of it."
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
		msg += fmt.Sprintf(": no type or package named %s in the workspace; if it is a dependency%s", first, outside)
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
		msg += fmt.Sprintf("\nNote: module(s) %s sit below the workspace without a go.work listing them, so gopls may not index their symbols. Open one of their files (outline or hover with its file_path) or add a go.work (go work init . ./<dir>).", strings.Join(names, ", "))
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
