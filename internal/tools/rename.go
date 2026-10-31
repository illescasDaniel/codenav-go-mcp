package tools

import (
	"context"
	"errors"
	"fmt"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/mentions"
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
	MaxNewErrors      *int   `json:"max_new_errors,omitempty" jsonschema:"default 0; -1 = no limit; other negative values are refused"`
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
	if in.Name != "" && (in.Line > 0 || in.Column > 0) {
		return nil, toolerr.Inputf("pass either name or file_path + line + column, not both (line/column would be ignored for name=%q).", in.Name)
	}
	var root member
	var kind int
	switch {
	case in.Name != "":
		sym, err := resolveSymbol(ctx, w, in.Name, in.FilePath)
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
			if identAt(abs, root.line, root.col) == "C" {
				if imps, _ := gosrc.ImportScope(abs, src); imps["C"] != "" {
					return nil, toolerr.Inputf("`C` is the cgo pseudo-package and can't be renamed.")
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
		return nil, renameFailure(w, root.path, err)
	}
	if rng == nil {
		if identAt(root.path, root.line, root.col) == "_" {
			return nil, toolerr.Inputf("_ (the blank identifier) cannot be renamed; it declares nothing. Give the value a name with edit instead.")
		}
		return nil, toolerr.Inputf("nothing renameable at %s:%d:%d.", edits.RelativeName(root.path, w.Root), root.line+1, root.col+1)
	}
	if in.Parameter == "" {
		// A use of a standard-library or dependency symbol: name the file that declares it (the plan's
		// first file would be an arbitrary one, such as doc.go).
		if defs, derr := w.Client.Definition(ctx, root.path, root.line+1, root.col+1); derr == nil && len(defs) > 0 {
			if dp := lsp.URIToPath(defs[0].URI); dp != "" && isCgoGenerated(dp) {
				return nil, toolerr.Inputf("%s is declared in cgo-generated code (it is C.%s); rename the C symbol in the cgo preamble or C sources instead.", identAt(root.path, root.line, root.col), identAt(root.path, root.line, root.col))
			} else if dp != "" && !w.inWorkspace(dp) {
				return nil, toolerr.Inputf("%s is declared in %s:%d, outside the workspace %s; only workspace symbols can be renamed.", identAt(root.path, root.line, root.col), dp, defs[0].Range.Start.Line+1, w.Root)
			}
		}
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
		if err := cgoExportRefusal(root.path, text, oldName); err != nil {
			return nil, err
		}
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
			return nil, renameFailure(w, root.path, err)
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
	if in.Parameter == "" && gosrc.IsEmbeddedFieldAt(root.path, text, edits.Offset(text, rng.Start.Line, rng.Start.Character)) {
		notes = append(notes, fmt.Sprintf("%s is an embedded field; renaming it also renames the type %s (and every other embedding of it).", oldName, oldName))
	}
	if in.Parameter == "" {
		if n := fieldTagNote(ctx, w, root.path, text, rng, oldName, in.NewName); n != "" {
			notes = append(notes, n)
		}
	}
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
		ms := mentions.FindScoped(w.Root, oldName, after, "", couldRefer(root.path, oldName))
		notes = append(notes, mentions.Notes(otherObjects(ctx, w, ms, after, oldName, root.path, kind >= 6 && kind <= 8), oldName)...)
	} else if ptext, ok := after[root.path]; ok {
		// A local, parameter or type parameter lives inside one declaration: only that span can
		// still mention the old name.
		off := edits.Offset(text, rng.Start.Line, rng.Start.Character)
		if a, b, ok := gosrc.EnclosingDecl(root.path, text, off); ok {
			from, to := 1+strings.Count(text[:a], "\n"), 1+strings.Count(text[:b], "\n")
			var inScope []mentions.Mention
			for _, m := range mentions.FindScoped(w.Root, oldName, map[string]string{root.path: ptext}, root.path, nil) {
				if m.Line >= from && m.Line <= to {
					inScope = append(inScope, m)
				}
			}
			notes = append(notes, mentions.Notes(inScope, oldName)...)
		}
	}
	return edits.NewPlan(desc, changes, notes...), nil
}

// otherObjects drops the code mentions that are some other, unrelated object. For a field or method,
// a bare identifier (a package-level func or variable of the same name, a local) can never be the
// member, so it goes whether or not its file changed. For the rest, a name gopls still resolves after
// the rename is another object; that needs the file's positions to be unchanged, so files the rename
// edits are kept.
func otherObjects(ctx context.Context, w *Workspace, ms []mentions.Mention, after map[string]string, name, declFile string, member bool) []mentions.Mention {
	out := ms[:0:0]
	checked := 0
	bare := map[string]map[int]bool{}      // abs path -> positions of non-member identifiers
	qualified := map[string]map[int]bool{} // abs path -> positions of identifiers that are not selected
	for _, m := range ms {
		abs := filepath.Join(w.Root, m.Path)
		if m.Kind != "code" || m.Col <= 0 {
			out = append(out, m)
			continue
		}
		if member {
			set, ok := bare[abs]
			if !ok {
				text, have := after[abs]
				if !have {
					text, _, _ = edits.ReadSource(abs)
				}
				set = gosrc.NonMemberIdents(abs, text, name)
				bare[abs] = set
			}
			if set[m.Line<<20|m.Col] {
				continue
			}
		} else if filepath.Dir(abs) != filepath.Dir(declFile) {
			// Another directory is another package: only `pkg.Name` can mean the renamed symbol
			// (a dot import aside).
			set, ok := qualified[abs]
			if !ok {
				text, have := after[abs]
				if !have {
					text, _, _ = edits.ReadSource(abs)
				}
				if !dotImport.MatchString(text) {
					set = gosrc.NonSelectorIdents(abs, text, name)
				}
				qualified[abs] = set
			}
			if set[m.Line<<20|m.Col] {
				continue
			}
		}
		if _, changed := after[abs]; !changed && checked < maxDefinitionChecks {
			checked++
			if defs, err := w.Client.Definition(ctx, abs, m.Line, m.Col); err == nil && len(defs) > 0 {
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

var dotImport = regexp.MustCompile(`(?m)^\s*(?:import\s+)?\.\s+"`)

// maxDefinitionChecks bounds the gopls round trips spent on telling unrelated mentions apart.
const maxDefinitionChecks = 200

// couldRefer accepts the Go files that could name the symbol declared in declFile: the files of its
// directory (including ones excluded by build constraints, which the type checker never saw) and,
// for an exported name, the files importing its package. Elsewhere a same-named identifier is
// another symbol. nil (no filter) when the package's import path is unknown.
func couldRefer(declFile, name string) func(path, text string) bool {
	dir := filepath.Dir(declFile)
	same := func(path string) bool { return filepath.Dir(path) == dir }
	if !token.IsExported(name) {
		return func(path, _ string) bool { return same(path) }
	}
	m, err := moduleOf(dir)
	if err != nil {
		return nil
	}
	ip, err := importPathOf(m, dir)
	if err != nil {
		return nil
	}
	quoted := strconv.Quote(ip)
	return func(path, text string) bool { return same(path) || strings.Contains(text, quoted) }
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
	var blockers, hidden []string
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
					label, exported := foreignInterface(n)
					if exported {
						blockers = append(blockers, label)
					} else {
						hidden = append(hidden, label)
					}
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
	if len(blockers)+len(hidden) > 0 {
		// Unexported foreign interfaces (the stdlib's private `stringer` copies) are noise next to fmt.Stringer.
		what := "an interface method of "
		list := blockers
		if len(list) == 0 {
			what, list = "a method of unexported interface(s) ", hidden
		}
		list = uniqueSorted(list)
		shown := strings.Join(list[:min(len(list), 3)], ", ")
		if len(list) > 3 {
			shown += fmt.Sprintf(" and %d more", len(list)-3)
		}
		return nil, 0, toolerr.Inputf("%s is also %s%s, outside the workspace (a type here satisfies a foreign interface); %s would break that relationship. "+
			"Pass linked=false (include_overrides=false) to change only this declaration and its uses.", name, what, shown, doing)
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

// identAt is the identifier covering line/col (0-based, UTF-16) in path, or "".
func identAt(path string, line, col int) string {
	src, _, err := edits.ReadSource(path)
	if err != nil {
		return ""
	}
	off := edits.Offset(src, line, col)
	isID := func(b byte) bool {
		return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
	}
	a, b := off, off
	for a > 0 && isID(src[a-1]) {
		a--
	}
	for b < len(src) && isID(src[b]) {
		b++
	}
	return src[a:b]
}

var (
	goplsLocRE      = regexp.MustCompile(`^\s*(\S+\.go):(\d+):(\d+):\s*(.*)$`)
	goplsConflictRE = regexp.MustCompile(`^conflicts with (\w+) in same block$`)
)

// renameFailure rewrites gopls' rename refusal ("tool/main.go:6:2: renaming this var ... conflicts with
// var in same block") with workspace-relative paths and the name of the conflicting declaration.
func renameFailure(w *Workspace, renamed string, err error) error {
	var re *lsp.RequestError
	if !errors.As(err, &re) || re.Code == -1 {
		return err
	}
	var out []string
	for i, ln := range strings.Split(strings.TrimSpace(re.Message), "\n") {
		m := goplsLocRE.FindStringSubmatch(ln)
		if m == nil {
			out = append(out, strings.TrimSpace(ln))
			continue
		}
		abs := goplsPath(w.Root, renamed, m[1])
		l, _ := strconv.Atoi(m[2])
		c, _ := strconv.Atoi(m[3])
		where := fmt.Sprintf("%s:%s:%s", edits.RelativeName(abs, w.Root), m[2], m[3])
		msg := m[4]
		if cm := goplsConflictRE.FindStringSubmatch(msg); cm != nil {
			msg = fmt.Sprintf("conflicts with %s %s in the same scope", cm[1], identAt(abs, l-1, c-1))
		}
		if i == 0 {
			out = append(out, fmt.Sprintf("Cannot rename: %s: %s", where, strings.TrimPrefix(msg, "renaming this ")))
		} else {
			out = append(out, fmt.Sprintf("  %s: %s", where, msg))
		}
	}
	return toolerr.Inputf("%s", strings.Join(out, "\n"))
}

// goplsPath maps a path from a gopls message (absolute, or relative to some directory gopls chose)
// to an absolute path in the workspace.
func goplsPath(root, renamed, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	// gopls prints "dir/file.go"; conflicts are in the renamed file's package, so look there first.
	suffix := string(filepath.Separator) + filepath.FromSlash(p)
	if strings.HasSuffix(renamed, suffix) {
		return renamed
	}
	if near := filepath.Join(filepath.Dir(renamed), filepath.Base(p)); strings.HasSuffix(near, suffix) {
		if _, err := os.Stat(near); err == nil {
			return near
		}
	}
	if _, err := os.Stat(filepath.Join(root, p)); err == nil {
		return filepath.Join(root, p)
	}
	found := ""
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if found != "" {
			return filepath.SkipAll
		}
		if err != nil {
			return nil
		}
		if d.IsDir() && path != root && (exclude.IsDirName(d.Name()) || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, suffix) {
			found = path
		}
		return nil
	})
	if found != "" {
		return found
	}
	return filepath.Join(root, p)
}

// foreignInterface labels the interface declaring the method at m ("fmt.Stringer"), falling back to
// its file:line; exported is false for an interface nobody outside its package can name.
func foreignInterface(m member) (label string, exported bool) {
	label = fmt.Sprintf("%s:%d", m.path, m.line+1)
	text, _, err := edits.ReadSource(m.path)
	if err != nil {
		return label, true
	}
	name := enclosingTypeName(m.path, text, edits.Offset(text, m.line, m.col))
	if name == "" {
		return label, true
	}
	return gosrc.PackageName(text) + "." + name, token.IsExported(name)
}

func uniqueSorted(xs []string) []string {
	out := append([]string(nil), xs...)
	sort.Strings(out)
	k := 0
	for i, x := range out {
		if i == 0 || x != out[k-1] {
			out[k] = x
			k++
		}
	}
	return out[:k]
}

// fieldTagNote says that the struct field being renamed carries a struct tag: gopls renames the Go
// identifier only, so the serialised name (json, db, yaml, xml, ...) stays as the tag has it. rng is the
// renamed identifier in text (file path); a rename started at a use is followed to the field's declaration.
func fieldTagNote(ctx context.Context, w *Workspace, path, text string, rng *lsp.Range, oldName, newName string) string {
	declPath, declText, off := path, text, edits.Offset(text, rng.Start.Line, rng.Start.Character)
	if defs, err := w.Client.Definition(ctx, path, rng.Start.Line+1, rng.Start.Character+1); err == nil && len(defs) > 0 {
		if p := lsp.URIToPath(defs[0].URI); p != "" {
			if src, _, rerr := edits.ReadSource(p); rerr == nil {
				declPath, declText, off = p, src, edits.Offset(src, defs[0].Range.Start.Line, defs[0].Range.Start.Character)
			}
		}
	}
	tag, found := gosrc.FieldTagAt(declPath, declText, off)
	if !found || tag == "" {
		return ""
	}
	extra := ""
	if gosrc.TagNamesField(tag, oldName) {
		extra = fmt.Sprintf(" (a tag value equals the old name %s, so it no longer matches the field)", oldName)
	}
	if gosrc.TagNamesField(tag, newName) {
		extra += fmt.Sprintf(" (a tag value already equals the new name %s)", newName)
	}
	return fmt.Sprintf("field %s has struct tags (%s); the tag values were not changed - serialised names stay the same / update them if the wire format should change%s.", oldName, gosrc.FormatTag(tag), extra)
}

// cgoExportRefusal refuses to rename a Go function that is exported to C with `//export name`: C code
// (the cgo preamble or .c/.h files) calls it by that name, and neither the directive nor those
// calls are Go identifiers the type checker can follow, so the rename would break the cgo build.
func cgoExportRefusal(path, text, name string) error {
	re := regexp.MustCompile(`(?m)^//export[ \t]+` + regexp.QuoteMeta(name) + `[ \t]*$`)
	loc := re.FindStringIndex(text)
	if loc == nil {
		return nil
	}
	dir := filepath.Dir(path)
	word := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
	var where []string
	where = append(where, fmt.Sprintf("%s:%d (//export %s)", filepath.Base(path), 1+strings.Count(text[:loc[0]], "\n"), name))
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		ext := filepath.Ext(e.Name())
		if e.IsDir() || (ext != ".go" && ext != ".c" && ext != ".h" && ext != ".cc" && ext != ".cpp") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		src := string(raw)
		inC := ext != ".go"
		if ext == ".go" {
			if !strings.Contains(src, `import "C"`) {
				continue
			}
			// Only the cgo preamble (the comment right above import "C") is C code.
			i := strings.Index(src, `import "C"`)
			start := strings.LastIndex(src[:i], "/*")
			if start < 0 || strings.Contains(src[start:i], "*/\n\n") {
				continue
			}
			src = src[start:i]
			inC = true
		}
		if !inC {
			continue
		}
		for n, ln := range strings.Split(src, "\n") {
			if word.MatchString(ln) {
				line := n + 1
				if ext == ".go" {
					line = 1 + strings.Count(string(raw)[:strings.Index(string(raw), src)], "\n") + n
				}
				where = append(where, fmt.Sprintf("%s:%d: %s", e.Name(), line, strings.TrimSpace(ln)))
			}
		}
	}
	return toolerr.Inputf("%s is exported to C (//export %s): C code calls it by name, and the directive and those calls are not Go identifiers the type checker follows, so renaming it would break the cgo build. Rename it by hand, changing all of these together:\n    %s",
		name, name, strings.Join(where, "\n    "))
}
