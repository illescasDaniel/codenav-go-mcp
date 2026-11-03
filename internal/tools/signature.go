package tools

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/buildscope"
	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

type addParam struct {
	Name     string `json:"name"`
	Type     string `json:"type" jsonschema:"Go type, e.g. int or context.Context"`
	Value    string `json:"value" jsonschema:"expression passed at every call site (Go has no default arguments)"`
	Position *int   `json:"position,omitempty" jsonschema:"0-based index in the final parameter list; default: the end (before a variadic parameter)"`
}

type changeSigIn struct {
	Name              string     `json:"name" jsonschema:"function or method, e.g. Scale, shapes.Scale or Square.Area"`
	FilePath          string     `json:"file_path,omitempty"`
	Add               []addParam `json:"add,omitempty"`
	Remove            []string   `json:"remove,omitempty" jsonschema:"parameter names; the matching argument is dropped at each call"`
	Reorder           []string   `json:"reorder,omitempty" jsonschema:"the complete new order of the remaining parameters"`
	Imports           []string   `json:"imports,omitempty" jsonschema:"imports needed by the added parameters' types or values"`
	IncludeOverrides  *bool      `json:"include_overrides,omitempty" jsonschema:"default true: change interface methods and implementations together"`
	Apply             *bool      `json:"apply,omitempty"`
	MaxNewErrors      *int       `json:"max_new_errors,omitempty" jsonschema:"default 0; -1 = no limit; other negative values are refused"`
	IncludeDependents *bool      `json:"include_dependents,omitempty"`
	AllowLarge        bool       `json:"allow_large,omitempty"`
}

// entry is one slot of the new parameter list: an existing parameter (old >= 0) or an added one.
type entry struct {
	old int
	add *addParam
}

func planOrder(sig *gosrc.Sig, in changeSigIn) ([]entry, error) {
	idx := map[string]int{}
	for i, p := range sig.Params {
		if p.Name == "" || p.Name == "_" {
			return nil, toolerr.Inputf("parameter %d has no usable name; name the parameters first (with edit_symbol)", i)
		}
		idx[p.Name] = i
	}
	removed := map[int]bool{}
	for _, n := range in.Remove {
		i, ok := idx[n]
		if !ok {
			return nil, toolerr.Inputf("no parameter %q (have: %s)", n, paramNames(sig))
		}
		removed[i] = true
	}
	var order []entry
	if len(in.Reorder) > 0 {
		seen := map[int]bool{}
		for _, n := range in.Reorder {
			i, ok := idx[n]
			switch {
			case !ok:
				return nil, toolerr.Inputf("reorder names unknown parameter %q (have: %s)", n, paramNames(sig))
			case removed[i]:
				return nil, toolerr.Inputf("parameter %q is both removed and reordered", n)
			case seen[i]:
				return nil, toolerr.Inputf("reorder lists %q twice", n)
			}
			seen[i] = true
			order = append(order, entry{old: i})
		}
		if len(order)+len(removed) != len(sig.Params) {
			return nil, toolerr.Inputf("reorder must list every remaining parameter (%s)", paramNames(sig))
		}
	} else {
		for i := range sig.Params {
			if !removed[i] {
				order = append(order, entry{old: i})
			}
		}
	}
	taken := map[string]bool{}
	for i, p := range sig.Params {
		if !removed[i] {
			taken[p.Name] = true
		}
	}
	for k := range in.Add {
		a := &in.Add[k]
		switch {
		case a.Name == "" || a.Type == "" || strings.TrimSpace(a.Value) == "":
			var miss []string
			for _, f := range []struct{ n, v string }{{"name", a.Name}, {"type", a.Type}, {"value", strings.TrimSpace(a.Value)}} {
				if f.v == "" {
					miss = append(miss, f.n)
				}
			}
			return nil, toolerr.Inputf("add[%d] is missing %s: every added parameter needs name, type and value (the argument for existing calls)", k, strings.Join(miss, ", "))
		case !token.IsIdentifier(a.Name):
			return nil, toolerr.Inputf("%q is not a valid Go parameter name (letters, digits and _, not starting with a digit, not a keyword)", a.Name)
		case taken[a.Name]:
			return nil, toolerr.Inputf("parameter %q already exists", a.Name)
		}
		if _, perr := parser.ParseExpr(a.Type); perr != nil {
			return nil, toolerr.Inputf("type %q of added parameter %s is not a valid Go type", a.Type, a.Name)
		}
		if _, perr := parser.ParseExpr(a.Value); perr != nil {
			return nil, toolerr.Inputf("value %q of added parameter %s is not a valid Go expression (it is written at every call site)", a.Value, a.Name)
		}
		taken[a.Name] = true
		pos := len(order)
		if n := len(order); n > 0 && sig.Params[order[n-1].old].Variadic && order[n-1].add == nil {
			pos = n - 1
		}
		if a.Position != nil {
			if *a.Position < 0 || *a.Position > len(order) {
				return nil, toolerr.Inputf("position %d is out of range (0..%d)", *a.Position, len(order))
			}
			pos = *a.Position
		}
		order = append(order[:pos], append([]entry{{old: -1, add: a}}, order[pos:]...)...)
	}
	for i, e := range order {
		if e.old >= 0 && sig.Params[e.old].Variadic && i != len(order)-1 {
			return nil, toolerr.Inputf("the variadic parameter %q must stay last", sig.Params[e.old].Name)
		}
	}
	return order, nil
}

func paramNames(sig *gosrc.Sig) string {
	var n []string
	for _, p := range sig.Params {
		n = append(n, p.Name)
	}
	return strings.Join(n, ", ")
}

// paramList renders the new parameter list for a member whose own parameters are `have`.
func paramList(have []gosrc.Param, order []entry) string {
	var parts []string
	var group []string
	groupType := ""
	flush := func() {
		if len(group) > 0 {
			parts = append(parts, strings.Join(group, ", ")+" "+groupType)
			group = nil
		}
	}
	for _, e := range order {
		name, typ, variadic := "", "", false
		if e.old >= 0 {
			name, typ, variadic = have[e.old].Name, have[e.old].Type, have[e.old].Variadic
		} else {
			name, typ = e.add.Name, e.add.Type
		}
		if len(group) > 0 && (typ != groupType || variadic) {
			flush()
		}
		group, groupType = append(group, name), typ
		if variadic {
			flush()
		}
	}
	flush()
	return strings.Join(parts, ", ")
}

// sigEdit replaces [start,end) with build(args), where args are the texts of the sub-ranges (which may
// themselves contain nested edits).
type sigEdit struct {
	start, end int
	args       [][2]int
	build      func(args []string) string
}

// render returns text[lo:hi] with the edits inside it applied; nested edits are applied inside the
// argument ranges of the edit that contains them.
func render(text string, lo, hi int, es []sigEdit) string {
	sort.Slice(es, func(i, j int) bool {
		return es[i].start < es[j].start || (es[i].start == es[j].start && es[i].end > es[j].end)
	})
	var b strings.Builder
	pos := lo
	for i := 0; i < len(es); {
		e := es[i]
		j := i + 1
		for j < len(es) && es[j].start < e.end {
			j++
		}
		if e.start < pos {
			i = j
			continue
		}
		b.WriteString(text[pos:e.start])
		args := make([]string, len(e.args))
		for k, r := range e.args {
			var inner []sigEdit
			for _, c := range es[i+1 : j] {
				if c.start >= r[0] && c.end <= r[1] {
					inner = append(inner, c)
				}
			}
			args[k] = render(text, r[0], r[1], inner)
		}
		b.WriteString(e.build(args))
		pos, i = e.end, j
	}
	b.WriteString(text[pos:hi])
	return b.String()
}

type fileState struct {
	path     string
	text     string
	bom      bool
	parse    *gosrc.File
	es       []sigEdit
	seen     map[int]bool // call sites already edited (references of linked methods overlap)
	inserted []string     // added types (declaration) and values (call sites) written into this file
}

func changeSignature(ctx context.Context, w *Workspace, in changeSigIn) (string, error) {
	if len(in.Add)+len(in.Remove)+len(in.Reorder) == 0 {
		return "", toolerr.Inputf("nothing to change: pass add, remove or reorder")
	}
	sym, err := resolveSymbol(ctx, w, in.Name, in.FilePath)
	if err != nil {
		return "", err
	}
	if sym.Kind != methodKind && sym.Kind != 12 && sym.Kind != 9 {
		return "", toolerr.Inputf("%s is %s, not a function or method.", in.Name, withArticle(lsp.SymbolKindNames[sym.Kind]))
	}
	root := member{lsp.URIToPath(sym.URI), sym.Line, sym.Column}
	if sym.Kind == 12 {
		if text, _, rerr := edits.ReadSource(root.path); rerr == nil {
			if d, derr := gosrc.FindDeclAt(root.path, text, edits.Offset(text, root.line, root.col)); derr == nil {
				if strings.HasSuffix(root.path, "_test.go") && !d.Method && isTestFunc(d.Name) {
					return "", toolerr.Inputf("%s is a test function: go test requires its fixed signature.", in.Name)
				}
				if sp := specialFunc(d, gosrc.PackageName(text)); sp != "" {
					return "", toolerr.Inputf("%s is the %s function: the Go toolchain requires it to take no parameters.", in.Name, sp)
				}
				if !d.Method {
					if where := cgoExportSites(w.Root, root.path, text, d.Name); where != nil {
						return "", toolerr.Inputf("%s is exported to C (//export %s): C code calls it with the prototype written in the cgo preamble or a header, which is not a Go declaration this tool can rewrite, so changing its signature would break the cgo build. Change it by hand, together with all of these:\n    %s", in.Name, d.Name, strings.Join(where, "\n    "))
					}
				}
			}
		}
	}
	members := []member{root}
	skipped := 0
	if sym.Kind == methodKind && flag(in.IncludeOverrides, true) {
		if members, skipped, err = linkedMembers(ctx, w, root, sym.Name, "changing its signature"); err != nil {
			return "", err
		}
	}

	files := map[string]*fileState{}
	state := func(p string) (*fileState, error) {
		if fs, ok := files[p]; ok {
			return fs, nil
		}
		abs, err := w.absPath(p)
		if err != nil {
			return nil, err
		}
		text, bom, err := edits.ReadSource(abs)
		if err != nil {
			return nil, err
		}
		pf, err := gosrc.ParseFile(abs, text)
		if err != nil {
			return nil, err
		}
		fs := &fileState{path: abs, text: text, bom: bom, parse: pf, seen: map[int]bool{}}
		files[p] = fs
		return fs, nil
	}

	var order []entry
	// Uses that need manual work, listed once per source line (with a count when a line has several).
	type manualLine struct {
		where string
		uses  int
		whys  []string
	}
	var manual []*manualLine
	manualByLine := map[string]*manualLine{}
	manualSeen := map[string]bool{} // linked members share references; count each use once
	manualUses := 0
	calls, dropped := 0, 0
	for mi, m := range members {
		fs, err := state(m.path)
		if err != nil {
			return "", err
		}
		sig, err := fs.parse.SigAt(edits.Offset(fs.text, m.line, m.col))
		if err != nil {
			return "", err
		}
		if mi == 0 {
			if order, err = planOrder(sig, in); err != nil {
				return "", err
			}
		}
		if sig.Unnamed {
			return "", toolerr.Inputf("%s:%d has unnamed parameters; name them first", edits.RelativeName(m.path, w.Root), m.line+1)
		}
		have := sig.Params
		for _, e := range order {
			if e.add != nil {
				fs.inserted = append(fs.inserted, e.add.Type)
			}
		}
		fs.es = append(fs.es, sigEdit{start: sig.Start, end: sig.End, build: func([]string) string { return paramList(have, order) }})

		refs, err := w.Client.References(ctx, m.path, m.line+1, m.col+1, false)
		if err != nil {
			return "", err
		}
		nparams, variadic := len(have), len(have) > 0 && have[len(have)-1].Variadic
		fixed := nparams
		if variadic {
			fixed--
		}
		for _, r := range refs {
			rfs, err := state(lsp.URIToPath(r.URI))
			if err != nil {
				return "", err
			}
			off := edits.Offset(rfs.text, r.Range.Start.Line, r.Range.Start.Character)
			where := fmt.Sprintf("%s:%d: %s", edits.RelativeName(rfs.path, w.Root), r.Range.Start.Line+1, rfs.parse.Line(off))
			c := rfs.parse.CallAt(off)
			addManual := func(why string) {
				key := fmt.Sprintf("%s:%d", rfs.path, off)
				if manualSeen[key] {
					return
				}
				manualSeen[key] = true
				manualUses++
				lk := fmt.Sprintf("%s:%d", rfs.path, r.Range.Start.Line)
				ml := manualByLine[lk]
				if ml == nil {
					ml = &manualLine{where: where}
					manualByLine[lk] = ml
					manual = append(manual, ml)
				}
				ml.uses++
				if !slices.Contains(ml.whys, why) {
					ml.whys = append(ml.whys, why)
				}
			}
			switch {
			case c == nil:
				addManual("used as a value, not called")
				continue
			case len(c.Args) < fixed || (!variadic && len(c.Args) != fixed):
				addManual("argument count does not match: f(g()) or a spread of several results")
				continue
			case c.Ellipsis && (!variadic || len(order) == 0 || order[len(order)-1].old != nparams-1):
				addManual("passes xs... to a variadic parameter that is changing")
				continue
			}
			if rfs.seen[c.Start] {
				continue
			}
			rfs.seen[c.Start] = true
			for i, h := range have {
				keep := false
				for _, e := range order {
					keep = keep || e.old == i
				}
				if !keep && !h.Variadic {
					dropped++
				}
			}
			for _, e := range order {
				if e.add != nil {
					rfs.inserted = append(rfs.inserted, e.add.Value)
				}
			}
			ord, fx := order, fixed
			build := func(args []string) string {
				var out []string
				for _, e := range ord {
					switch {
					case e.old < 0:
						out = append(out, strings.TrimSpace(e.add.Value))
					case e.old < fx:
						out = append(out, args[e.old])
					default:
						out = append(out, args[fx:]...)
					}
				}
				return strings.Join(out, ", ")
			}
			argTexts := make([]string, len(c.Args))
			for k, r := range c.Args {
				argTexts[k] = rfs.text[r[0]:r[1]]
			}
			if build(argTexts) != strings.Join(argTexts, ", ") { // a no-op reorder leaves the call as it is
				calls++
			}
			rfs.es = append(rfs.es, sigEdit{start: c.Start, end: c.End, args: c.Args, build: build})
		}
	}

	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	declImports, _ := gosrc.ImportScope(files[root.path].path, files[root.path].text)
	var importNotes []string
	var changes []edits.FileChange
	for _, p := range paths {
		fs := files[p]
		if len(fs.es) == 0 {
			continue
		}
		next := render(fs.text, 0, len(fs.text), fs.es)
		need, inotes, err := neededImports(w, fs, in.Imports, declImports)
		if err != nil {
			return "", err
		}
		importNotes = append(importNotes, inotes...)
		if next, err = gosrc.EditImports(fs.path, next, "", need, false); err != nil {
			return "", err
		}
		old := fs.text
		c, err := fileChange(fs.path, next, &old, fs.bom)
		if err != nil {
			return "", err
		}
		if !c.IsNoop() {
			changes = append(changes, c)
		}
	}

	notes := []string{fmt.Sprintf("Updated %d call site(s).", calls)}
	if len(changes) == 0 {
		notes = []string{"No change: the parameter list is already as requested."}
	}
	notes = append(notes, importNotes...)
	if len(members) > 1 {
		notes = append(notes, fmt.Sprintf("Changed %d interface/implementing method declarations together.", len(members)))
	}
	if n := skippedNote(skipped, sym.Name); n != "" {
		notes = append(notes, n)
	}
	if dropped > 0 && len(in.Remove) > 0 {
		notes = append(notes, fmt.Sprintf("%d argument expression(s) were dropped; check they had no side effects.", dropped))
	}
	if len(manual) > 0 {
		lines := make([]string, min(len(manual), 12))
		for i := range lines {
			ml := manual[i]
			why := strings.Join(ml.whys, "; ")
			if ml.uses > 1 {
				why = fmt.Sprintf("%d uses: %s", ml.uses, why)
			}
			lines[i] = ml.where + "  (" + why + ")"
		}
		notes = append(notes, fmt.Sprintf("%d use(s) need manual work:\n    %s", manualUses, strings.Join(lines, "\n    ")))
	}
	holds, excluded := excludedCheck(w, root, identAt(root.path, root.line, root.col), sym.Kind == methodKind)
	notes = append(notes, excluded...)
	plan := edits.NewPlan("change_signature "+in.Name, changes, notes...)
	plan.Holds = holds
	return finishPlan(ctx, w, plan, flag(in.Apply, true), limit(in.MaxNewErrors), flag(in.IncludeDependents, true), in.AllowLarge)
}

// excludedCheck covers what gopls cannot see: the uses and declarations of the function in files the go tool
// leaves out of this build (buildscope). Those are not rewritten, so they hold the write (a preview that
// apply_edit writes). A method's selectors cannot be told apart by name, so they are only noted.
func excludedCheck(w *Workspace, decl member, name string, method bool) (holds, notes []string) {
	dir := filepath.Dir(decl.path)
	ip := ""
	if m, err := moduleOf(dir); err == nil {
		ip, _ = importPathOf(m, dir)
	}
	var uses, decls, sels []string
	for _, h := range buildscope.Mentions(buildscope.Context(), buildscope.Query{
		Root: w.Root, DeclFile: decl.path, Name: name, Member: method, ImportPath: ip,
	}) {
		if h.Kind == buildscope.Asm || (h.Path == decl.path && h.Line-1 == decl.line) {
			continue
		}
		where := fmt.Sprintf("%s:%d (%s)", edits.RelativeName(h.Path, w.Root), h.Line, h.Reason)
		switch h.Kind {
		case buildscope.Use:
			uses = append(uses, where)
		case buildscope.Decl:
			decls = append(decls, where)
		case buildscope.Member:
			sels = append(sels, where)
		}
	}
	if len(uses) > 0 {
		holds = append(holds, fmt.Sprintf("%d use(s) of %s in files the build leaves out are not rewritten: %s.", len(uses), name, listing(uses)))
	}
	if len(decls) > 0 {
		holds = append(holds, fmt.Sprintf("%s is declared again in files the build leaves out: %s.", name, listing(decls)))
	}
	if len(holds) > 0 {
		holds = append(holds, "Change those files too, or check them with the build that includes them (GOOS=... go build, or -tags in CODENAV_GO_MCP_BUILD_FLAGS).")
	}
	if len(sels) > 0 {
		notes = append(notes, fmt.Sprintf("%d selector(s) of %s in files the build leaves out were not checked: %s.", len(sels), name, listing(sels)))
	}
	return holds, notes
}

// listing joins up to five entries and says how many were left out.
func listing(items []string) string {
	if len(items) <= 5 {
		return strings.Join(items, "; ")
	}
	return fmt.Sprintf("%s; and %d more", strings.Join(items[:5], "; "), len(items)-5)
}

// neededImports lists the imports the added types and values written into fs need and fs lacks.
// A qualifier is resolved from the imports= argument, then from the declaring file's imports,
// then from the standard library; one that stays unknown is left for the type check to report.
func neededImports(w *Workspace, fs *fileState, given []string, declImports map[string]string) (need, notes []string, err error) {
	have, declared := gosrc.ImportScope(fs.path, fs.text)
	pkg := gosrc.PackageName(fs.text)
	userSpecs := map[string]string{}
	for _, g := range given {
		alias, p, perr := gosrc.ParseImportSpec(g)
		if perr != nil {
			return nil, nil, perr
		}
		name := alias
		if name == "" {
			name = gosrc.ImportName(p)
		}
		userSpecs[name] = strings.TrimSpace(alias + " " + strconv.Quote(p))
	}
	done := map[string]bool{}
	for _, expr := range fs.inserted {
		for _, q := range gosrc.Qualifiers(expr) {
			if done[q] {
				continue
			}
			done[q] = true
			if _, ok := have[q]; ok || declared[q] {
				continue
			}
			spec, ok := userSpecs[q]
			if !ok {
				spec, ok = declImports[q]
			}
			if !ok {
				var ambiguous bool
				if spec, ambiguous = gosrc.StdlibImport(q); ambiguous {
					return nil, nil, toolerr.Inputf("%q in %q matches several standard packages; pass imports=[...] with the one you mean.", q, expr)
				}
			}
			switch {
			case spec == "":
			case q == pkg:
				notes = append(notes, fmt.Sprintf("%s: %q refers to the file's own package; not imported.", edits.RelativeName(fs.path, w.Root), q))
			default:
				need = append(need, spec)
			}
		}
	}
	return need, notes, nil
}

// withArticle puts "a" or "an" before word.
func withArticle(word string) string {
	if word != "" && strings.ContainsRune("aeiouAEIOU", rune(word[0])) {
		return "an " + word
	}
	return "a " + word
}
