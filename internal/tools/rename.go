package tools

import (
	"context"
	"fmt"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/mentions"
	"github.com/illescasDaniel/codenav-go-mcp/internal/resolve"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

const (
	methodKind = 6
	maxLinked  = 40
)

type renameIn struct {
	NewName           string `json:"new_name"`
	Name              string `json:"name,omitempty" jsonschema:"symbol, e.g. Square.Area or shapes.Scale"`
	FilePath          string `json:"file_path,omitempty" jsonschema:"narrows name, or the file of line/column"`
	Line              int    `json:"line,omitempty" jsonschema:"1-indexed, with file_path and column"`
	Column            int    `json:"column,omitempty" jsonschema:"1-indexed UTF-16 offset"`
	Parameter         string `json:"parameter,omitempty" jsonschema:"rename this parameter of the function or method given by name (or at line/column) instead of the function itself"`
	Linked            *bool  `json:"linked,omitempty" jsonschema:"default true: rename the interface method and every implementation together"`
	Apply             *bool  `json:"apply,omitempty"`
	MaxNewErrors      *int   `json:"max_new_errors,omitempty"`
	IncludeDependents *bool  `json:"include_dependents,omitempty"`
	AllowLarge        bool   `json:"allow_large,omitempty"`
}

// member is one declaration that has to change with the rename (0-based LSP position of its name).
type member struct {
	path      string
	line, col int
}

func (m member) key() string { return fmt.Sprintf("%s:%d:%d", m.path, m.line, m.col) }

func kindAt(syms []lsp.Symbol, line, col int) int {
	var walk func(s *lsp.Symbol) int
	walk = func(s *lsp.Symbol) int {
		for _, c := range s.Children {
			if k := walk(c); k != 0 {
				return k
			}
		}
		if r := s.SelectionRange; r != nil && r.Start.Line == line && r.Start.Character <= col && col <= r.End.Character && r.End.Line == line {
			return s.Kind
		}
		return 0
	}
	for i := range syms {
		if k := walk(&syms[i]); k != 0 {
			return k
		}
	}
	return 0
}

func addEdits(into map[string][]lsp.TextEdit, from map[string][]lsp.TextEdit) {
	for p, es := range from {
		have := map[lsp.TextEdit]bool{}
		for _, e := range into[p] {
			have[e] = true
		}
		for _, e := range es {
			if !have[e] {
				into[p] = append(into[p], e)
				have[e] = true
			}
		}
	}
}

func (w *Workspace) inWorkspace(path string) bool {
	rel, err := filepath.Rel(w.Root, path)
	return err == nil && !strings.HasPrefix(rel, "..") && !exclude.IsExcluded(path, w.Root)
}

// packageDir finds a directory below root named name that holds .go files ("" if none).
func packageDir(root, name string) string {
	if name == "" || strings.ContainsAny(name, "./") {
		return ""
	}
	found := ""
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" || !d.IsDir() {
			return nil
		}
		if path != root && (exclude.IsDirName(d.Name()) || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if d.Name() == name {
			if m, _ := filepath.Glob(filepath.Join(path, "*.go")); len(m) > 0 {
				found, _ = filepath.Rel(root, path)
				found = filepath.ToSlash(found)
			}
		}
		return nil
	})
	return found
}

func packageRenameError(w *Workspace, pkg, file string) error {
	dir := edits.RelativeName(filepath.Dir(file), w.Root)
	return toolerr.Inputf("%s is a package, not a symbol; renaming packages is not supported - use move on the package directory (file_path=%s, to_file=<new dir>)", pkg, dir)
}

// planRename renames the member and, when linked, everything gopls' rename leaves behind that
// would break the program: implementations of an interface method, and the interface methods a
// concrete method satisfies, followed transitively.
func planRename(ctx context.Context, w *Workspace, in renameIn) (*edits.EditPlan, error) {
	if !token.IsIdentifier(in.NewName) {
		return nil, toolerr.Inputf("%q is not a valid Go identifier (it must not be a keyword either).", in.NewName)
	}
	var root member
	var kind int
	switch {
	case in.Name != "":
		sym, err := resolve.Resolve(ctx, w.Client, w.Root, in.Name, in.FilePath)
		if err != nil {
			if dir := packageDir(w.Root, in.Name); dir != "" && strings.HasPrefix(err.Error(), "No symbol found") {
				return nil, toolerr.Inputf("%s is a package, not a symbol; renaming packages is not supported - use move on the package directory (file_path=%s, to_file=<new dir>)", in.Name, dir)
			}
			return nil, err
		}
		root = member{lsp.URIToPath(sym.URI), sym.Line, sym.Column}
		kind = sym.Kind
	case in.FilePath != "" && in.Line > 0 && in.Column > 0:
		abs, err := w.absPath(in.FilePath)
		if err != nil {
			return nil, err
		}
		root = member{abs, in.Line - 1, in.Column - 1}
		if src, _, rerr := edits.ReadSource(abs); rerr == nil && in.Parameter == "" {
			if a, b, ok := gosrc.PackageNameRange(src); ok {
				if off := edits.Offset(src, root.line, root.col); off >= a && off <= b {
					return nil, packageRenameError(w, src[a:b], abs)
				}
			}
		}
		if syms, err := w.Client.DocumentSymbol(ctx, abs); err == nil {
			kind = kindAt(syms, root.line, root.col)
		}
	default:
		return nil, toolerr.Inputf("pass `name` (e.g. Square.Area) or `file_path` + `line` + `column`.")
	}

	if in.Parameter != "" {
		var err error
		if root, err = parameterOf(root, in.Parameter, w); err != nil {
			return nil, err
		}
		kind = 0
	}

	rng, err := w.Client.PrepareRename(ctx, root.path, root.line+1, root.col+1)
	if err != nil {
		if strings.Contains(err.Error(), "can't rename package") {
			if src, _, rerr := edits.ReadSource(root.path); rerr == nil {
				return nil, packageRenameError(w, gosrc.PackageName(src), root.path)
			}
		}
		return nil, err
	}
	if rng == nil {
		return nil, toolerr.Inputf("nothing renameable at %s:%d:%d.", edits.RelativeName(root.path, w.Root), root.line+1, root.col+1)
	}
	text, _, err := edits.ReadSource(root.path)
	if err != nil {
		return nil, err
	}
	oldName := text[edits.Offset(text, rng.Start.Line, rng.Start.Character):edits.Offset(text, rng.End.Line, rng.End.Character)]
	if oldName == in.NewName {
		return nil, toolerr.Inputf("%s is already called %s.", oldName, in.NewName)
	}

	if in.Parameter == "" {
		if d, derr := gosrc.FindDeclAt(root.path, text, edits.Offset(text, rng.Start.Line, rng.Start.Character)); derr == nil {
			if sp := specialFunc(d, gosrc.PackageName(text)); sp != "" {
				return nil, toolerr.Inputf("%s is the %s function; renaming it would break the program (go build %s).", oldName, sp, map[string]string{"main": "finds no entry point", "init": "no longer runs it at start-up"}[sp])
			}
			if strings.HasSuffix(root.path, "_test.go") && d.Keyword == "func" && !d.Method && isTestFunc(oldName) && !isTestFunc(in.NewName) {
				return nil, toolerr.Inputf("%s is a test function; %s is not a valid test name, so go test would stop running it (keep the Test/Benchmark/Fuzz/Example prefix followed by an upper-case letter or digit).", oldName, in.NewName)
			}
			if in.NewName == "init" && d.Keyword == "func" && !d.Method {
				return nil, toolerr.Inputf("a top-level func named init is run by the runtime and cannot be called; pick another name.")
			}
		}
	}

	// The members that must change together. gopls checks each rename on its own and refuses
	// to break an interface relationship, so for several members the edits come from references.
	members := []member{root}
	skipped := 0
	if flag(in.Linked, true) && kind == methodKind {
		var err error
		if members, skipped, err = linkedMembers(ctx, w, root, oldName, "renaming it"); err != nil {
			return nil, err
		}
	}

	all := map[string][]lsp.TextEdit{}
	linked := members[1:]
	if len(members) == 1 {
		we, err := w.Client.Rename(ctx, root.path, root.line+1, root.col+1, in.NewName)
		if err != nil {
			return nil, err
		}
		by, err := edits.ParseWorkspaceEdit(we)
		if err != nil {
			return nil, err
		}
		addEdits(all, by)
	} else {
		for _, m := range members {
			locs, err := w.Client.References(ctx, m.path, m.line+1, m.col+1, true)
			if err != nil {
				return nil, err
			}
			by := map[string][]lsp.TextEdit{}
			for _, l := range locs {
				p := lsp.URIToPath(l.URI)
				by[p] = append(by[p], lsp.TextEdit{Range: l.Range, NewText: in.NewName})
			}
			addEdits(all, by)
		}
	}

	changes, err := edits.ChangesFromEdits(all)
	if err != nil {
		return nil, err
	}
	for i := range changes {
		if c := &changes[i]; c.New != nil && strings.HasSuffix(c.Path, ".go") {
			if fc, err := fileChange(c.Path, *c.New, c.Old, c.OldBOM); err == nil {
				*c = fc
			}
		}
	}
	desc := fmt.Sprintf("rename %s -> %s", oldName, in.NewName)
	var notes []string
	if len(linked) > 0 {
		var lines []string
		seenLine := map[string]bool{}
		for _, m := range linked {
			l := fmt.Sprintf("    %s:%d", edits.RelativeName(m.path, w.Root), m.line+1)
			if !seenLine[l] {
				seenLine[l] = true
				lines = append(lines, l)
			}
		}
		sort.Strings(lines)
		more := ""
		if len(lines) > 10 {
			more = fmt.Sprintf("\n    ... and %d more", len(lines)-10)
			lines = lines[:10]
		}
		notes = append(notes, fmt.Sprintf("Also renamed %d linked method(s) (interface methods and implementations rename together):\n%s%s", len(linked), strings.Join(lines, "\n"), more))
	} else if kind == methodKind && flag(in.Linked, true) {
		notes = append(notes, "No interface relationships found for this method.")
	}
	if n := skippedNote(skipped, oldName); n != "" {
		notes = append(notes, n)
	}
	after := map[string]string{}
	for _, c := range changes {
		if c.New != nil {
			after[c.Path] = *c.New
		}
	}
	// A parameter or local variable is fully resolved by gopls; same-named text elsewhere is unrelated.
	if in.Parameter == "" && kind != 0 {
		notes = append(notes, mentions.Notes(mentions.Find(w.Root, oldName, after, ""), oldName)...)
	}
	return edits.NewPlan(desc, changes, notes...), nil
}

// linkedMembers returns root plus every method that has to change with it: the interface methods it
// satisfies and the implementations of those, transitively (via textDocument/implementation).
//
// A method outside the workspace cannot be changed. When the member being expanded is a concrete
// method, what gopls returns are interface methods it satisfies: one outside the workspace (fmt.Stringer,
// error) would break, so the operation is refused. When it is an interface method, what comes back are
// implementers: those outside the workspace (any stdlib type with a Name() string) are not touched and
// not expanded; their number is returned as skipped.
func linkedMembers(ctx context.Context, w *Workspace, root member, name, doing string) (members []member, skipped int, err error) {
	members = []member{root}
	seen := map[string]bool{root.key(): true}
	texts := map[string]string{}
	concrete := func(m member) bool {
		text, ok := texts[m.path]
		if !ok {
			text, _, _ = edits.ReadSource(m.path)
			texts[m.path] = text
		}
		d, err := gosrc.FindDeclAt(m.path, text, edits.Offset(text, m.line, m.col))
		return err == nil && d.Keyword == "func" && d.Method
	}
	var blockers []string
	for i := 0; i < len(members); i++ {
		m := members[i]
		locs, err := w.Client.Implementation(ctx, m.path, m.line+1, m.col+1)
		if err != nil {
			return nil, 0, err
		}
		isConcrete := concrete(m)
		for _, l := range locs {
			n := member{lsp.URIToPath(l.URI), l.Range.Start.Line, l.Range.Start.Character}
			if seen[n.key()] {
				continue
			}
			seen[n.key()] = true
			if !w.inWorkspace(n.path) {
				if isConcrete {
					blockers = append(blockers, fmt.Sprintf("%s:%d", edits.RelativeName(n.path, w.Root), n.line+1))
				} else {
					skipped++
				}
				continue
			}
			if len(members) > maxLinked {
				return nil, 0, toolerr.Inputf("more than %d linked methods; pass linked=false (include_overrides=false) to change only this one.", maxLinked)
			}
			members = append(members, n)
		}
	}
	if len(blockers) > 0 {
		shown := strings.Join(blockers[:min(len(blockers), 3)], ", ")
		if len(blockers) > 3 {
			shown += fmt.Sprintf(" and %d more", len(blockers)-3)
		}
		return nil, 0, toolerr.Inputf("%s is also an interface method in %s, outside the workspace (a type here satisfies a foreign interface); %s would break that relationship. "+
			"Pass linked=false (include_overrides=false) to change only this declaration and its uses.", name, shown, doing)
	}
	return members, skipped, nil
}

func skippedNote(n int, name string) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d type(s) outside the workspace also have a %s method; they are unaffected.", n, name)
}

// parameterOf moves root from a function's name to the name of its parameter.
func parameterOf(fn member, param string, w *Workspace) (member, error) {
	text, _, err := edits.ReadSource(fn.path)
	if err != nil {
		return member{}, err
	}
	f, err := gosrc.ParseFile(fn.path, text)
	if err != nil {
		return member{}, err
	}
	sig, err := f.SigAt(edits.Offset(text, fn.line, fn.col))
	if err != nil {
		return member{}, toolerr.Inputf("parameter= needs a function or method: %v", err)
	}
	var names []string
	for _, p := range sig.Params {
		if p.Name == param {
			line, col := edits.PositionOf(text, p.NameOff)
			return member{fn.path, line, col}, nil
		}
		if p.Name != "" {
			names = append(names, p.Name)
		}
	}
	if len(names) == 0 {
		return member{}, toolerr.Inputf("the parameters of this function are unnamed or blank.")
	}
	return member{}, toolerr.Inputf("no parameter %q; it has: %s.", param, strings.Join(names, ", "))
}
