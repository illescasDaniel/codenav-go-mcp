package tools

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

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
	var skipped []string
	if sym.Kind == methodKind && flag(in.IncludeOverrides, true) {
		if members, skipped, err = linkedMembers(ctx, w, root, sym.Name, "changing its signature", "include_overrides=false"); err != nil {
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
	var droppedComments []string // comments inside the arguments of removed arguments
	var reorderSites []string    // calls whose side-effecting arguments the reorder runs in another order
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
		memberOff := edits.Offset(fs.text, m.line, m.col)
		for _, e := range order {
			if e.add == nil {
				continue
			}
			if line := fs.parse.BodyDeclLine(memberOff, e.add.Name); line > 0 {
				return "", toolerr.Inputf("parameter %q would clash with the local %q declared at %s:%d in the body of %s: choose another name for the parameter, or rename the local first", e.add.Name, e.add.Name, edits.RelativeName(m.path, w.Root), line, in.Name)
			}
			use, err := bodyCapture(ctx, w, fs, memberOff, e.add.Name)
			if err != nil {
				return "", err
			}
			if use != "" {
				return "", toolerr.Inputf("parameter %q would take over the %q used at %s in the body of %s, and that use does not resolve to a local of the body: choose another name for the parameter", e.add.Name, e.add.Name, use, in.Name)
			}
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
			// A method expression (*T).M(t, args...) passes the receiver as argument 0, before the parameters.
			shift := 0
			if c != nil && c.XOff >= 0 {
				isType, known := qualifierIsType(ctx, w, rfs, c)
				if !known {
					addManual("cannot tell whether the qualifier names a type: check by hand")
					continue
				}
				if isType {
					shift = 1
				}
			}
			switch {
			case c == nil:
				addManual("used as a value, not called")
				continue
			case len(c.Args) < fixed+shift || (!variadic && len(c.Args) != fixed+shift):
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
			argTexts := make([]string, len(c.Args))
			for k, r := range c.Args {
				argTexts[k] = rfs.text[r[0]:r[1]]
			}
			slots := callSlots(order, fixed, shift, len(c.Args))
			identity := len(slots) == len(c.Args)
			for i, s := range slots {
				identity = identity && s.arg == i
			}
			if identity { // nothing moves, is added or is removed: the call stays as it is
				continue
			}
			if reordersEffects(slots, argTexts) {
				reorderSites = append(reorderSites, fmt.Sprintf("%s:%d", edits.RelativeName(rfs.path, w.Root), r.Range.Start.Line+1))
			}
			if renderCall(slots, argTexts, callGaps{}, "") == strings.Join(argTexts, ", ") {
				continue // the same texts in the same order: the call stays as written, layout and comments included
			}
			calls++
			gaps := gapComments(rfs.text, c)
			kept := map[int]bool{}
			for _, s := range slots {
				kept[s.arg] = true
			}
			for k, cm := range gaps.comments {
				if kept[k] {
					continue
				}
				droppedComments = append(droppedComments, cm.lead...)
				if cm.trail != "" {
					droppedComments = append(droppedComments, cm.trail)
				}
			}
			indent := ""
			if gaps.layout && len(c.Args) > 0 {
				indent = argIndent(rfs.text, c.Args[0][0])
			}
			build := func(args []string) string { return renderCall(slots, args, gaps, indent) }
			rfs.es = append(rfs.es, sigEdit{start: c.Start, end: gaps.end, args: c.Args, build: build})
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
		links, err := satisfactionNotes(ctx, w, root, members)
		if err != nil {
			return "", err
		}
		notes = append(notes, links...)
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
	if len(droppedComments) > 0 {
		notes = append(notes, fmt.Sprintf("%d comment(s) inside a call were dropped with a removed argument: %s.", len(droppedComments), listing(droppedComments)))
	}
	holds, excluded := buildGuard(w, root, identAt(root.path, root.line, root.col), "rewritten")
	notes = append(notes, excluded...)
	if len(reorderSites) > 0 {
		holds = append(holds, fmt.Sprintf("The reorder changes the order in which %d call(s) evaluate their arguments: %s. Go evaluates the arguments of a call from left to right, so each call that has side effects now feeds another parameter.", len(reorderSites), listing(reorderSites)))
	}
	if h := foreignHold(skipped, sym.Name); h != "" {
		holds = append(holds, h)
	}
	plan := edits.NewPlan("change_signature "+in.Name, changes, notes...)
	plan.Holds = holds
	return finishPlan(ctx, w, plan, flag(in.Apply, true), limit(in.MaxNewErrors), flag(in.IncludeDependents, true), in.AllowLarge)
}

// listing joins up to five entries and says how many were left out.
func listing(items []string) string {
	if len(items) <= 5 {
		return strings.Join(items, "; ")
	}
	return fmt.Sprintf("%s; and %d more", strings.Join(items[:5], "; "), len(items)-5)
}

// satisfactionNotes says why each member outside the root's directory changes with the root (SIG-001). Such a
// member is reached through a type that satisfies both interfaces, and nothing in the file list shows that link:
// "b.Store.Get changed too because a.Mem implements both a.Store and b.Store". Members of the root's own
// directory need no note. Where the link is not that simple the note says only that the two are linked.
func satisfactionNotes(ctx context.Context, w *Workspace, root member, members []member) ([]string, error) {
	rootText, _, err := edits.ReadSource(root.path)
	if err != nil {
		return nil, err
	}
	rootOwner, _ := ifaceMethod(root.path, rootText, edits.Offset(rootText, root.line, root.col))
	// The types that satisfy the root's interface directly (only meaningful when the root is an interface method).
	direct := map[string]bool{}
	if rootOwner != "" {
		locs, err := w.Client.Implementation(ctx, root.path, root.line+1, root.col+1)
		if err != nil {
			return nil, err
		}
		for _, l := range locs {
			direct[member{lsp.URIToPath(l.URI), l.Range.Start.Line, l.Range.Start.Character}.key()] = true
		}
	}
	var notes []string
	sat := map[string]map[string]bool{} // concrete member -> the members its implementations list (one query each)
	for _, n := range members[1:] {
		if filepath.Dir(n.path) == filepath.Dir(root.path) {
			continue
		}
		nText, _, err := edits.ReadSource(n.path)
		if err != nil {
			return nil, err
		}
		nOwner, _ := ifaceMethod(n.path, nText, edits.Offset(nText, n.line, n.col))
		nLabel := memberLabel(n.path, nText, n.line, n.col)
		// gopls finds the interfaces a concrete method satisfies across packages, not the methods that satisfy an
		// interface of another package: so the search starts from the concrete members outside n's directory.
		var impl *member // a concrete member, outside n's directory, that satisfies n
		for _, c := range members {
			if filepath.Dir(c.path) == filepath.Dir(n.path) {
				continue
			}
			cText, _, err := edits.ReadSource(c.path)
			if err != nil {
				return nil, err
			}
			if !isConcreteMethod(c.path, cText, c.line, c.col) {
				continue
			}
			if sat[c.key()] == nil {
				locs, err := w.Client.Implementation(ctx, c.path, c.line+1, c.col+1)
				if err != nil {
					return nil, err
				}
				sat[c.key()] = map[string]bool{}
				for _, l := range locs {
					sat[c.key()][member{lsp.URIToPath(l.URI), l.Range.Start.Line, l.Range.Start.Character}.key()] = true
				}
			}
			if sat[c.key()][n.key()] {
				impl = &c
				break
			}
		}
		switch {
		case impl == nil || nOwner == "":
			notes = append(notes, fmt.Sprintf("%s changed too: it is linked to %s through interface satisfaction.", nLabel, memberLabel(root.path, rootText, root.line, root.col)))
		case rootOwner != "" && direct[impl.key()]:
			implText, _, err := edits.ReadSource(impl.path)
			if err != nil {
				return nil, err
			}
			implType := typeLabel(impl.path, implText, impl.line, impl.col)
			notes = append(notes, fmt.Sprintf("%s changed too because %s implements both %s.%s and %s.%s.",
				nLabel, implType, gosrc.PackageName(rootText), rootOwner, gosrc.PackageName(nText), nOwner))
		case rootOwner == "" && impl.key() == root.key():
			notes = append(notes, fmt.Sprintf("%s changed too because %s satisfies %s.%s.",
				nLabel, memberLabel(root.path, rootText, root.line, root.col), gosrc.PackageName(nText), nOwner))
		default:
			notes = append(notes, fmt.Sprintf("%s changed too: it is linked to %s through interface satisfaction.", nLabel, memberLabel(root.path, rootText, root.line, root.col)))
		}
	}
	return notes, nil
}

// ifaceMethod names the interface type and the method that the identifier at byte offset off of src is in
// ("Store", "Get"); both are "" when it is not an interface method.
func ifaceMethod(path, src string, off int) (owner, method string) {
	fset := token.NewFileSet()
	f, _ := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if f == nil {
		return "", ""
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			it, ok := ts.Type.(*ast.InterfaceType)
			if !ok || it.Methods == nil {
				continue
			}
			for _, field := range it.Methods.List {
				for _, id := range field.Names {
					if fset.Position(id.Pos()).Offset == off {
						return ts.Name.Name, id.Name
					}
				}
			}
		}
	}
	return "", ""
}

// memberLabel names the member at line, col of path the way the note reads it: "b.Store.Get" for an interface
// method, "a.Mem.Get" for a method, "a.Name" for a function.
func memberLabel(path, src string, line, col int) string {
	pkg := gosrc.PackageName(src)
	if owner, name := ifaceMethod(path, src, edits.Offset(src, line, col)); owner != "" {
		return pkg + "." + owner + "." + name
	}
	name := identAt(path, line, col)
	if d, err := gosrc.FindDeclAt(path, src, edits.Offset(src, line, col)); err == nil && d.Method {
		return pkg + "." + d.Recv + "." + name
	}
	return pkg + "." + name
}

// typeLabel names the receiver type of the method at line, col of path: "a.Mem".
func typeLabel(path, src string, line, col int) string {
	pkg := gosrc.PackageName(src)
	if d, err := gosrc.FindDeclAt(path, src, edits.Offset(src, line, col)); err == nil && d.Method {
		return pkg + "." + d.Recv
	}
	return pkg + "." + identAt(path, line, col)
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

// callSlot is one argument of a rewritten call: the index of an existing argument, or -1 for an added
// parameter, whose value is written instead.
type callSlot struct {
	arg   int
	value string
}

// callSlots lists the arguments of a call of n arguments in the new parameter order. shift is 1 for a method
// expression (T.M(recv, args...)): the receiver, argument 0, stays first and the parameters follow it.
// fixed is the number of parameters before a variadic one.
func callSlots(order []entry, fixed, shift, n int) []callSlot {
	var out []callSlot
	if shift == 1 {
		out = append(out, callSlot{arg: 0})
	}
	for _, e := range order {
		switch {
		case e.add != nil:
			out = append(out, callSlot{arg: -1, value: strings.TrimSpace(e.add.Value)})
		case e.old < fixed:
			out = append(out, callSlot{arg: e.old + shift})
		default:
			for k := fixed + shift; k < n; k++ {
				out = append(out, callSlot{arg: k})
			}
		}
	}
	return out
}

// argComments are the comments written between a call's arguments: the comment after argument k's comma on
// its line (trail), and the comments on their own lines before it (lead).
type argComments struct {
	lead  []string
	trail string
}

// callGaps is what the text between a call's arguments holds besides the arguments.
type callGaps struct {
	comments []argComments
	layout   bool // a line break or a comment sits between two arguments: one argument per line
	// end is the end of the replaced range: the last argument, or the comment after its comma when that
	// comment is on the same line (tailOpen). The comments before the first argument stay where they are.
	end      int
	tailOpen bool
}

// gapComments reads the comments between the arguments of call c (text is the file text).
func gapComments(text string, c *gosrc.Call) callGaps {
	n := len(c.Args)
	g := callGaps{comments: make([]argComments, n), end: c.End}
	add := func(a int, cm string, afterBreak bool) {
		if afterBreak {
			g.comments[a+1].lead = append(g.comments[a+1].lead, cm)
		} else {
			g.comments[a].trail = strings.TrimSpace(g.comments[a].trail + " " + cm)
		}
		g.layout = true
	}
	for a := 0; a+1 < n; a++ {
		gap := text[c.Args[a][1]:c.Args[a+1][0]]
		afterBreak := false
		for i := 0; i < len(gap); {
			switch {
			case gap[i] == '\n':
				afterBreak = true
				g.layout = true
				i++
			case strings.HasPrefix(gap[i:], "//"):
				end := strings.IndexByte(gap[i:], '\n')
				if end < 0 {
					end = len(gap) - i
				}
				add(a, strings.TrimRight(gap[i:i+end], " \t\r"), afterBreak)
				i += end
			case strings.HasPrefix(gap[i:], "/*"):
				stop := len(gap)
				if end := strings.Index(gap[i+2:], "*/"); end >= 0 {
					stop = i + 2 + end + 2
				}
				add(a, gap[i:stop], afterBreak)
				i = stop
			default:
				i++
			}
		}
	}
	if n > 0 {
		if end, cm, ok := tailComment(text, c.Args[n-1][1]); ok {
			g.end, g.tailOpen, g.layout = end, true, true
			g.comments[n-1].trail = cm
		}
	}
	return g
}

// tailComment finds a comment on the line of the last argument, after the comma that follows it. It returns
// the end of that comment. The comma and the comment are replaced with the call, which writes its own comma.
func tailComment(text string, from int) (end int, cm string, ok bool) {
	i := from
	for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
		i++
	}
	if i >= len(text) || text[i] != ',' {
		return 0, "", false
	}
	i++
	for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
		i++
	}
	switch {
	case strings.HasPrefix(text[i:], "//"):
		stop := strings.IndexByte(text[i:], '\n')
		if stop < 0 {
			stop = len(text) - i
		}
		cm = strings.TrimRight(text[i:i+stop], " \t\r")
		return i + len(cm), cm, true
	case strings.HasPrefix(text[i:], "/*"):
		stop := strings.Index(text[i:], "*/")
		if stop < 0 {
			return 0, "", false
		}
		cm = text[i : i+stop+2]
		if strings.Contains(cm, "\n") {
			return 0, "", false
		}
		return i + len(cm), cm, true
	}
	return 0, "", false
}

// renderCall writes the arguments of a rewritten call. Without layout they are joined on one line. With layout
// each argument starts a line (indented by indent) with the comments written next to it. A comment after the
// comma of the last argument would hide the closing text when it ends the line, so it moves in front of the
// argument. A block comment may stay after the comma the rewritten text writes (tailOpen); a line comment may not,
// because the closing parenthesis would follow it on the same line.
func renderCall(slots []callSlot, args []string, g callGaps, indent string) string {
	texts := make([]string, len(slots))
	for i, s := range slots {
		if s.arg < 0 {
			texts[i] = s.value
		} else {
			texts[i] = args[s.arg]
		}
	}
	if !g.layout {
		return strings.Join(texts, ", ")
	}
	var b strings.Builder
	for i, s := range slots {
		var lead []string
		trail := ""
		if s.arg >= 0 {
			lead, trail = g.comments[s.arg].lead, g.comments[s.arg].trail
		}
		last := i == len(slots)-1
		// A line comment that ends the output needs a line break after it: the source after the range has one only
		// when the comment is the tail of the original last argument.
		if last && trail != "" && (!g.tailOpen || strings.HasPrefix(trail, "//") && s.arg != len(args)-1) {
			lead, trail = append(slices.Clone(lead), trail), ""
		}
		if i > 0 {
			b.WriteString("\n" + indent)
		}
		for _, cm := range lead {
			b.WriteString(cm + "\n" + indent)
		}
		b.WriteString(texts[i])
		if !last || g.tailOpen {
			b.WriteString(",")
		}
		if trail != "" {
			b.WriteString(" " + trail)
		}
	}
	return b.String()
}

// argIndent is the indentation of the line where the argument at off starts. An argument that shares its line
// with the call is indented one level further.
func argIndent(text string, off int) string {
	ls := strings.LastIndexByte(text[:off], '\n') + 1
	end := ls
	for end < off && (text[end] == ' ' || text[end] == '\t') {
		end++
	}
	if end < off {
		return text[ls:end] + "\t"
	}
	return text[ls:end]
}

// sideEffecting reports whether an argument calls a function or receives from a channel outside a function
// literal: the operations Go evaluates left to right. Text that does not parse counts as having effects.
func sideEffecting(text string) bool {
	e, err := parser.ParseExpr(text)
	if err != nil {
		return true
	}
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			found = true
		case *ast.UnaryExpr:
			found = found || t.Op == token.ARROW
		}
		return !found
	})
	return found
}

// reordersEffects reports whether the new order runs two arguments that have effects in another order than the
// call wrote them (Go evaluates them left to right, so the value each one produces changes).
func reordersEffects(slots []callSlot, argTexts []string) bool {
	for p := range slots {
		for q := p + 1; q < len(slots); q++ {
			a, b := slots[p].arg, slots[q].arg
			if a < 0 || b < 0 || a < b {
				continue
			}
			if sideEffecting(argTexts[a]) && sideEffecting(argTexts[b]) {
				return true
			}
		}
	}
	return false
}

// qualifierIsType reports whether the qualifier of call c names a type, as in T.M(recv, args...) or (*T).M(...),
// which passes the receiver as argument 0. gopls resolves the qualifier, so a variable that shadows a type name is a
// value. known is false when gopls does not resolve the qualifier to exactly one declaration.
func qualifierIsType(ctx context.Context, w *Workspace, fs *fileState, c *gosrc.Call) (isType, known bool) {
	line, ch := edits.PositionOf(fs.text, c.XOff)
	locs, err := w.Client.Definition(ctx, fs.path, line+1, ch+1)
	if err != nil || len(locs) != 1 {
		return false, false
	}
	path, text, df := lsp.URIToPath(locs[0].URI), fs.text, fs.parse
	if path != fs.path {
		var rerr error
		if text, _, rerr = edits.ReadSource(path); rerr != nil {
			return false, false
		}
		if df, rerr = gosrc.ParseFile(path, text); rerr != nil {
			return false, false
		}
	}
	decl := edits.Offset(text, locs[0].Range.Start.Line, locs[0].Range.Start.Character)
	return df.TypeNameAt(decl), true
}

// bodyCapture returns where the body of the function at memberOff uses name without a declaration of its own in the
// body, so that an added parameter of that name would take the use over; "" when there is none. gopls resolves
// each use: a declaration inside the body is a local, anything else (a package-level name, an import, a predeclared
// name, a parameter being removed) is not, and a use gopls cannot resolve counts as one.
func bodyCapture(ctx context.Context, w *Workspace, fs *fileState, memberOff int, name string) (string, error) {
	start, end, uses, ok := fs.parse.BodyUses(memberOff, name)
	if !ok {
		return "", nil
	}
	for _, off := range uses {
		line, ch := edits.PositionOf(fs.text, off)
		locs, err := w.Client.Definition(ctx, fs.path, line+1, ch+1)
		if err == nil && len(locs) == 1 && lsp.URIToPath(locs[0].URI) == fs.path {
			decl := edits.Offset(fs.text, locs[0].Range.Start.Line, locs[0].Range.Start.Character)
			if start <= decl && decl < end {
				continue
			}
		}
		return fmt.Sprintf("%s:%d", edits.RelativeName(fs.path, w.Root), line+1), nil
	}
	return "", nil
}

// withArticle puts "a" or "an" before word.
func withArticle(word string) string {
	if word != "" && strings.ContainsRune("aeiouAEIOU", rune(word[0])) {
		return "an " + word
	}
	return "a " + word
}
