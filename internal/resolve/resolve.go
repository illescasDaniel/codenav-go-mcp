// Package resolve turns a name ("Square", "Square.Area", "shapes.Scale") into one symbol position.
package resolve

import (
	"context"
	"path"
	"path/filepath"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/format"
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

func candidateLines(cands []*lsp.Symbol, root string) string {
	lines := make([]string, len(cands))
	for i, c := range cands {
		lines[i] = "  " + format.FormatWorkspaceSymbol(c, root)
		if c.ContainerName != "" {
			lines[i] += "  in " + c.ContainerName
		}
	}
	return strings.Join(lines, "\n")
}

// Resolve finds the one symbol named query, optionally restricted to file (workspace-relative).
func Resolve(ctx context.Context, c *lsp.Client, root, query, file string) (Symbol, error) {
	query = strings.TrimSpace(query)
	last := query
	if i := strings.LastIndex(query, "."); i >= 0 {
		last = query[i+1:]
	}
	syms, err := c.WorkspaceSymbol(ctx, last)
	if err != nil {
		return Symbol{}, err
	}
	var all []*lsp.Symbol
	for i := range syms {
		if syms[i].Location != nil {
			all = append(all, &syms[i])
		}
	}
	var exact, loose []*lsp.Symbol
	for _, s := range format.Rank(all, last) {
		switch matches(s, query) {
		case 2:
			exact = append(exact, s)
		case 1:
			loose = append(loose, s)
		}
	}
	// A case-exact match beats a merely case-insensitive one.
	cands := exact
	if len(cands) == 0 {
		cands = loose
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
			return Symbol{}, toolerr.Inputf("No symbol %q in %q; %d match(es) elsewhere:\n%s", query, file, len(cands), candidateLines(cands, root))
		}
		cands = narrowed
	}
	switch len(cands) {
	case 0:
		hint := ""
		if !strings.Contains(query, ".") {
			hint = " (struct fields and methods are named Type.Name)"
		}
		if strings.Contains(query, ".") {
			hint = " (methods promoted through embedding are not declared symbols; query the embedded type's method instead)"
		}
		return Symbol{}, toolerr.Inputf("No symbol found matching %q%s.", query, hint)
	case 1:
		return fromLSP(cands[0]), nil
	}
	sameFile := true
	for _, c := range cands {
		if c.Location.URI != cands[0].Location.URI {
			sameFile = false
		}
	}
	hint := "qualify it with the package (`shapes.Scale`) or pass file_path to disambiguate"
	if sameFile {
		hint = "these all live in the same file; use search_symbol to get exact line/column, then hover/definition/references with that position"
	}
	return Symbol{}, toolerr.Inputf("%d symbols match %q; %s:\n%s", len(cands), query, hint, candidateLines(cands, root))
}
