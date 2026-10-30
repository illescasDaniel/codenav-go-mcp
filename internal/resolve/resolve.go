// Package resolve turns a name ("Square", "Square.Area", "shapes.Scale") into one symbol position.
package resolve

import (
	"context"
	"fmt"
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

// NotFoundError is a name that matches no workspace symbol. It reads as an input error; callers
// that know more about the language can replace the hint (see Query).
type NotFoundError struct {
	Query string
	Err   *toolerr.InputError
}

func (e *NotFoundError) Error() string { return e.Err.Msg }
func (e *NotFoundError) Unwrap() error { return e.Err }

// SymbolCap is gopls's limit on workspace/symbol results; a result list this long may have
// dropped the exact match in favour of fuzzy ones.
const SymbolCap = 100

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

// Resolve finds the one symbol named query, optionally restricted to file (workspace-relative).
func Resolve(ctx context.Context, c *lsp.Client, root, query, file string) (Symbol, error) {
	query = strings.TrimSpace(query)
	last := query
	if i := strings.LastIndex(query, "."); i >= 0 {
		last = query[i+1:]
	}
	all, err := lookup(ctx, c, query, last)
	if err != nil {
		return Symbol{}, err
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
		return Symbol{}, &NotFoundError{Query: query, Err: &toolerr.InputError{Msg: fmt.Sprintf("No symbol found matching %q%s.", query, hint)}}
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
