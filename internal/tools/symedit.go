package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/format"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/resolve"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

type editSymbolIn struct {
	Action            string   `json:"action" jsonschema:"replace, insert or delete"`
	Name              string   `json:"name,omitempty" jsonschema:"symbol, e.g. Square.Area or shapes.Scale"`
	Source            string   `json:"source,omitempty" jsonschema:"the complete new definition (replace, insert)"`
	FilePath          string   `json:"file_path,omitempty" jsonschema:"narrows name; REQUIRED for action=insert: the file the new code goes into"`
	Position          string   `json:"position,omitempty" jsonschema:"insert: after (default with name), before, into (struct or interface body) or end (default without name)"`
	Imports           []string `json:"imports,omitempty" jsonschema:"imports the new code needs, e.g. \"strings\" or \"js \\\"encoding/json\\\"\""`
	Force             bool     `json:"force,omitempty" jsonschema:"delete even when the symbol is still used"`
	PruneImports      *bool    `json:"prune_imports,omitempty" jsonschema:"default true: drop imports this edit leaves unused"`
	Apply             *bool    `json:"apply,omitempty"`
	MaxNewErrors      *int     `json:"max_new_errors,omitempty"`
	IncludeDependents *bool    `json:"include_dependents,omitempty"`
}

type located struct {
	path, text string
	orig       string // the file as on disk; differs from text after carveOut
	bom        bool
	sym        resolve.Symbol
	decl       *gosrc.Decl
	unbuilt    bool // the file is excluded from the current build (gopls does not know it)
}

// declKeys are the forms in which a file declares name: "Scale" for shapes.Scale, "Square.Area" for a method.
func declKeys(name, pkg string) []string {
	parts := strings.Split(strings.TrimSpace(name), ".")
	switch len(parts) {
	case 1:
		return parts
	case 2:
		if parts[0] == pkg {
			return []string{parts[1], name}
		}
		return []string{name}
	default:
		return []string{strings.Join(parts[len(parts)-2:], ".")}
	}
}

// siblingDecls lists "file:line" for every .go file next to path, of the same package, that declares name.
// gopls only indexes the files of the current build, so build-constrained twins are invisible to Resolve.
func siblingDecls(w *Workspace, path, text, name string) []string {
	pkg := gosrc.PackageName(text)
	entries, _ := os.ReadDir(filepath.Dir(path))
	var found []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".go" {
			continue
		}
		p := filepath.Join(filepath.Dir(path), e.Name())
		src := text
		if p != path {
			var err error
			if src, _, err = edits.ReadSource(p); err != nil || gosrc.PackageName(src) != pkg {
				continue
			}
		}
		for _, k := range declKeys(name, pkg) {
			if _, off, err := gosrc.FindDeclByName(p, src, k); err == nil {
				line, _ := edits.PositionOf(src, off)
				found = append(found, fmt.Sprintf("%s:%d", edits.RelativeName(p, w.Root), line+1))
				break
			}
		}
	}
	return found
}

func locate(ctx context.Context, w *Workspace, name, file string) (*located, error) {
	sym, err := resolve.Resolve(ctx, w.Client, w.Root, name, file)
	unbuilt := false
	if err != nil && file != "" && strings.HasPrefix(err.Error(), "No symbol") {
		// The file may be excluded from the current build (a build tag): gopls does not index it.
		if abs, aerr := w.absPath(file); aerr == nil {
			if text, _, rerr := edits.ReadSource(abs); rerr == nil {
				for _, k := range declKeys(name, gosrc.PackageName(text)) {
					if d, off, derr := gosrc.FindDeclByName(abs, text, k); derr == nil {
						line, col := edits.PositionOf(text, off)
						sym = resolve.Symbol{Name: d.Name, URI: lsp.PathToURI(abs), Line: line, Column: col}
						err, unbuilt = nil, true
						break
					}
				}
			}
		}
	}
	if err != nil {
		return nil, err
	}
	abs, err := w.absPath(lsp.URIToPath(sym.URI))
	if err != nil {
		return nil, err
	}
	text, bom, err := edits.ReadSource(abs)
	if err != nil {
		return nil, err
	}
	if file == "" {
		if where := siblingDecls(w, abs, text, name); len(where) > 1 {
			return nil, toolerr.Inputf("%s is declared in %d files (build-constrained): %s; pass file_path.", name, len(where), strings.Join(where, ", "))
		}
	}
	d, err := gosrc.FindDeclAt(abs, text, edits.Offset(text, sym.Line, sym.Column))
	if err != nil {
		return nil, err
	}
	return &located{path: abs, text: text, orig: text, bom: bom, sym: sym, decl: d, unbuilt: unbuilt}, nil
}

// specialFunc names the functions the Go toolchain gives a fixed meaning: "main" for the top-level
// func main of package main, "init" for a top-level func init, "" for anything else.
func specialFunc(d *gosrc.Decl, pkg string) string {
	if d == nil || d.Keyword != "func" || d.Method {
		return ""
	}
	switch {
	case d.Name == "main" && pkg == "main":
		return "main"
	case d.Name == "init":
		return "init"
	}
	return ""
}

// isTestFunc reports whether name is one go test treats specially: Test, Benchmark, Fuzz or Example,
// alone or followed by something that does not start with a lower-case letter.
func isTestFunc(name string) bool {
	for _, prefix := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if rest, ok := strings.CutPrefix(name, prefix); ok {
			r, _ := utf8.DecodeRuneInString(rest)
			return rest == "" || !unicode.IsLower(r)
		}
	}
	return false
}

// stripKeyword lets a grouped-spec replacement be given as `const B = 2` or just `B = 2`.
func stripKeyword(src, keyword string) string {
	if rest, ok := strings.CutPrefix(src, keyword+" "); ok && !strings.HasPrefix(strings.TrimSpace(rest), "(") {
		return strings.TrimSpace(rest)
	}
	return src
}

func checkSnippet(src string, d *gosrc.Decl) error {
	wrapped := "package p\n" + src
	if d != nil && d.Grouped {
		wrapped = "package p\n" + d.Keyword + " (\n" + src + "\n)"
	}
	return gosrc.CheckSyntax("source.go", wrapped)
}

func lineStart(text string, off int) int { return strings.LastIndex(text[:off], "\n") + 1 }

func lineEnd(text string, off int) int {
	i := strings.Index(text[off:], "\n")
	if i < 0 {
		return len(text)
	}
	return off + i + 1
}

func editSymbol(ctx context.Context, w *Workspace, in editSymbolIn) (string, error) {
	apply, maxNew := flag(in.Apply, true), limit(in.MaxNewErrors)
	prune := flag(in.PruneImports, true)
	source := strings.Trim(strings.ReplaceAll(in.Source, "\r\n", "\n"), "\n")
	var plan *edits.EditPlan
	var err error
	switch in.Action {
	case "replace":
		if in.Name == "" || strings.TrimSpace(source) == "" {
			return "", toolerr.Inputf(`action="replace" needs name and source.`)
		}
		plan, err = replaceSymbol(ctx, w, in, source, prune)
	case "insert":
		if strings.TrimSpace(source) == "" || in.FilePath == "" {
			return "", toolerr.Inputf(`action="insert" needs source and file_path.`)
		}
		plan, err = insertSymbol(ctx, w, in, source)
	case "delete":
		if in.Name == "" {
			return "", toolerr.Inputf(`action="delete" needs name.`)
		}
		if strings.TrimSpace(in.Source) != "" {
			return "", toolerr.Inputf("delete takes no source; did you mean action=replace?")
		}
		var refused string
		plan, refused, err = deleteSymbol(ctx, w, in, prune)
		if err == nil && refused != "" {
			return refused, nil
		}
	default:
		return "", toolerr.Inputf("unknown action %q: use replace, insert or delete.", in.Action)
	}
	if err != nil {
		return "", err
	}
	return finishPlan(ctx, w, plan, apply, maxNew, flag(in.IncludeDependents, true), false)
}

func replaceSymbol(ctx context.Context, w *Workspace, in editSymbolIn, source string, prune bool) (*edits.EditPlan, error) {
	l, err := locate(ctx, w, in.Name, in.FilePath)
	if err != nil {
		return nil, err
	}
	if l.decl.MultiName {
		return nil, toolerr.Inputf("%s is declared together with other names; change it with edit.", in.Name)
	}
	if l.decl.Grouped {
		source = stripKeyword(source, l.decl.Keyword)
	}
	if err := checkSnippet(source, l.decl); err != nil {
		return nil, err
	}
	if sp := specialFunc(l.decl, gosrc.PackageName(l.text)); sp != "" {
		if names, nerr := gosrc.DeclNames(source); nerr == nil && len(names) == 1 && names[0] != l.decl.Name {
			return nil, toolerr.Inputf("%s is the %s function; the replacement is called %s, which would remove it. Keep the name %s.", in.Name, sp, names[0], l.decl.Name)
		}
	}
	next := l.text[:l.decl.Start] + edits.MatchEOL(source, l.text) + l.text[l.decl.End:]
	next, err = gosrc.EditImports(l.path, next, l.text, in.Imports, prune)
	if err != nil {
		return nil, err
	}
	var notes []string
	if l.decl.HasDoc && !strings.HasPrefix(source, "//") && !strings.HasPrefix(source, "/*") {
		notes = append(notes, "The old doc comment was dropped because source has none; include it in source to keep it.")
	}
	if names, nerr := gosrc.DeclNames(source); nerr == nil && !l.decl.Grouped && len(names) == 1 && !strings.HasSuffix("."+names[0], "."+l.decl.Name) {
		notes = append(notes, fmt.Sprintf("The new definition is called %s, not %s; its callers are not renamed (use rename_symbol).", names[0], l.decl.Name))
	}
	return planFile(l.path, "replace "+in.Name, next, l.text, true, l.bom, notes...)
}

func insertSymbol(ctx context.Context, w *Workspace, in editSymbolIn, source string) (*edits.EditPlan, error) {
	abs, err := w.absPath(in.FilePath)
	if err != nil {
		return nil, err
	}
	if filepath.Ext(abs) != ".go" {
		return nil, toolerr.Inputf("%q is not a .go file.", in.FilePath)
	}
	text, bom, rerr := edits.ReadSource(abs)
	exists := rerr == nil
	if rerr != nil && !os.IsNotExist(rerr) {
		return nil, rerr
	}
	where := in.Position
	if where == "" {
		where = "end"
		if in.Name != "" {
			where = "after"
		}
	}
	switch {
	case where == "end" && in.Name != "":
		return nil, toolerr.Inputf(`position="end" appends to the file; drop name, or use after/before/into.`)
	case where != "end" && in.Name == "":
		return nil, toolerr.Inputf("position=%q needs name (the symbol to place it relative to).", where)
	case where != "end" && where != "after" && where != "before" && where != "into":
		return nil, toolerr.Inputf("unknown position %q: use after, before, into or end.", where)
	}
	if !exists {
		if where != "end" {
			return nil, toolerr.Inputf("%s does not exist; insert with position=end creates it.", in.FilePath)
		}
		text = "package " + packageFor(abs) + "\n"
	}
	wholeFile := false
	if pk := gosrc.PackageName(source); pk != "" {
		want := gosrc.PackageName(text)
		if pk != want && !(pk == want+"_test" && strings.HasSuffix(abs, "_test.go")) {
			return nil, toolerr.Inputf("source is package %s, file is package %s.", pk, want)
		}
		if !exists {
			wholeFile = true
		} else {
			imps, body, serr := gosrc.SplitFile("source.go", source)
			if serr != nil {
				return nil, serr
			}
			if strings.TrimSpace(body) == "" {
				return nil, toolerr.Inputf("source has a package clause but no declaration.")
			}
			in.Imports = append(append([]string(nil), in.Imports...), imps...)
			source = body
		}
	}
	next := ""
	var d *gosrc.Decl
	if where != "end" {
		l, err := locate(ctx, w, in.Name, in.FilePath)
		if err != nil {
			return nil, err
		}
		if l.path != abs {
			return nil, toolerr.Inputf("%s is declared in %s, not %s.", in.Name, edits.RelativeName(l.path, w.Root), in.FilePath)
		}
		d = l.decl
	}
	switch {
	case wholeFile:
		next = edits.MatchEOL(strings.TrimRight(source, "\r\n")+"\n", text)
	case where == "into":
		if d.Closing < 0 {
			return nil, toolerr.Inputf("%s is not a struct or interface type; `into` adds fields or interface methods.", in.Name)
		}
		wrapped := "package p\ntype T " + d.BodyKind + " {\n" + source + "\n}"
		if err := gosrc.CheckSyntax("source.go", wrapped); err != nil {
			return nil, err
		}
		next = insertMember(text, d.Closing, edits.MatchEOL(source, text))
	default:
		if err := checkSnippet(source, nil); err != nil {
			return nil, err
		}
		if d != nil && d.Grouped {
			return nil, toolerr.Inputf("%s is inside a const/var/type block; place the new code relative to a standalone declaration.", in.Name)
		}
		if names, _ := gosrc.DeclNames(source); len(names) > 0 {
			have := map[string]bool{}
			for _, n := range gosrc.FileDeclNames(abs, text) {
				have[n] = true
			}
			for _, n := range names {
				if have[n] {
					return nil, toolerr.Inputf("%s is already declared in %s; use action=replace.", n, in.FilePath)
				}
			}
		}
		src := edits.MatchEOL(source, text)
		eol := edits.MatchEOL("\n\n", text)
		switch where {
		case "before":
			next = text[:d.Start] + src + eol + text[d.Start:]
		case "after":
			next = text[:d.End] + eol + src + text[d.End:]
		default:
			next = strings.TrimRight(text, "\r\n") + eol + src + edits.MatchEOL("\n", text)
		}
	}
	next, err = gosrc.EditImports(abs, next, "", in.Imports, false)
	if err != nil {
		return nil, err
	}
	return planFile(abs, "insert into "+in.FilePath, next, text, exists, bom)
}

// packageFor guesses the package clause of a new file from its siblings, else the directory name.
func packageFor(abs string) string {
	entries, _ := os.ReadDir(filepath.Dir(abs))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".go" || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		if raw, err := os.ReadFile(filepath.Join(filepath.Dir(abs), e.Name())); err == nil {
			if n := gosrc.PackageName(string(raw)); n != "" {
				return n
			}
		}
	}
	return strings.NewReplacer("-", "_", ".", "_").Replace(filepath.Base(filepath.Dir(abs)))
}

func deleteSymbol(ctx context.Context, w *Workspace, in editSymbolIn, prune bool) (*edits.EditPlan, string, error) {
	l, err := locate(ctx, w, in.Name, in.FilePath)
	if err != nil {
		return nil, "", err
	}
	d := l.decl
	if d.MultiName {
		return nil, "", toolerr.Inputf("%s is declared together with other names; change it with edit.", in.Name)
	}
	sp := specialFunc(d, gosrc.PackageName(l.text))
	if sp == "main" && !in.Force {
		return nil, "", toolerr.Inputf("%s is the program's entry point (func main in package main); deleting it breaks go build. Pass force=true to delete it anyway.", in.Name)
	}
	var locs []lsp.Location
	var notes []string
	if l.unbuilt {
		notes = append(notes, fmt.Sprintf("%s is excluded from the current build, so uses of %s were not checked.", edits.RelativeName(l.path, w.Root), in.Name))
	} else if locs, err = w.Client.References(ctx, l.path, l.sym.Line+1, l.sym.Column+1, false); err != nil {
		return nil, "", err
	}
	var users []lsp.Location
	for _, r := range locs {
		if lsp.URIToPath(r.URI) == l.path {
			off := edits.Offset(l.text, r.Range.Start.Line, r.Range.Start.Character)
			if off >= d.Start && off < d.End {
				continue // the definition referring to itself
			}
		}
		users = append(users, r)
	}
	if len(users) > 0 && !in.Force {
		return nil, fmt.Sprintf("Not deleted: %s is still used in %d place(s) (force=true deletes it anyway and shows what breaks):\n%s",
			in.Name, len(users), format.FormatReferences(users, w.Root, format.DefaultReferencesSnippetLimit)), nil
	}
	from, to := d.Start, d.End
	from, to = lineStart(l.text, from), lineEnd(l.text, to)
	next := l.text[:from] + l.text[to:]
	next, err = gosrc.EditImports(l.path, next, l.text, nil, prune)
	if err != nil {
		return nil, "", err
	}
	if d.Grouped && strings.Contains(blockText(l.text, d.Start, d.End), "iota") {
		notes = append(notes, "The block uses iota: the values of the constants after the deleted one shift.")
	}
	if sp == "init" {
		notes = append(notes, "init functions run at program start; its side effects are gone with it.")
	}
	if len(users) > 0 {
		notes = append(notes, fmt.Sprintf("Forced: %d use(s) of %s remain.", len(users), in.Name))
	}
	plan, err := planFile(l.path, "delete "+in.Name, next, l.text, true, l.bom, notes...)
	return plan, "", err
}

// carveOut rewrites the located file in memory so that a grouped or multi-name symbol stands alone.
func carveOut(l *located) (*located, error) {
	carved, off, err := gosrc.CarveSpec(l.path, l.text, edits.Offset(l.text, l.sym.Line, l.sym.Column))
	if err != nil {
		return nil, err
	}
	d, err := gosrc.FindDeclAt(l.path, carved, off)
	if err != nil {
		return nil, err
	}
	c := *l
	c.text, c.decl = carved, d
	return &c, nil
}

// blockText is the parenthesised const/var/type block around a grouped spec.
func blockText(text string, start, end int) string {
	a := strings.LastIndex(text[:start], "\n)")
	b := strings.LastIndex(text[:start], " (\n")
	if a > b {
		b = a
	}
	if b < 0 {
		b = 0
	}
	e := strings.Index(text[end:], "\n)")
	if e < 0 {
		return text[b:]
	}
	return text[b : end+e]
}

// insertMember puts member before the closing brace at offset closing; it adds no blank line
// when the brace already starts its own line.
func insertMember(text string, closing int, member string) string {
	before := text[:closing]
	if !strings.HasSuffix(before, "\n") {
		before += "\n"
	}
	return before + member + "\n" + text[closing:]
}
