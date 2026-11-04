package tools

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/buildscope"
	"github.com/illescasDaniel/codenav-go-mcp/internal/diagdelta"
	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
	"github.com/illescasDaniel/codenav-go-mcp/internal/format"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
	"github.com/illescasDaniel/codenav-go-mcp/internal/index"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// CheckLimit caps how many files are type-checked per simulation.
const CheckLimit = 60

// maxDeltaCauseLookups caps the go list lookups that name the causes of broken imports in one comparison.
const maxDeltaCauseLookups = 5

// causeNamer words the broken imports of the diagnostics of one comparison with their causes, as the diagnostics
// tool does (explainBrokenImports). Both sides share its budget, its lookups and its answers: a broken import then
// reads the same before and after the edit, and is not reported as new on one side and fixed on the other.
type causeNamer struct {
	ctx     context.Context
	root    string
	lookups int
	known   map[string]string // file + "\x00" + import path -> cause ("" when unknown)
}

// explain returns items with the causes of their broken imports (file is the file the diagnostics belong to).
func (c *causeNamer) explain(file string, items []lsp.Diagnostic) []lsp.Diagnostic {
	abs := file
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(c.root, abs)
	}
	return format.ExplainBrokenImports(items, func(p string) string {
		key := abs + "\x00" + p
		if v, ok := c.known[key]; ok {
			return v
		}
		if c.lookups >= maxDeltaCauseLookups {
			return ""
		}
		c.lookups++
		v := gomod.ImportCause(c.ctx, c.root, abs, p)
		c.known[key] = v
		return v
	})
}

// collect pulls diagnostics for paths (skipping `skip`), labelling them with the text in
// `overlays` when present, else the disk's. Call inside client.Overlay for the overlaid side.
func collect(ctx context.Context, w *Workspace, overlays map[string]string, paths []string, skip map[string]bool, causes *causeNamer) ([]diagdelta.Entry, []string) {
	var entries []diagdelta.Entry
	var unchecked []string
	for _, p := range paths {
		if skip[p] {
			continue
		}
		text, ok := overlays[p]
		if !ok {
			b, _, err := edits.ReadSource(p)
			if err != nil {
				continue // not on disk and not overlaid: nothing on this side
			}
			text = b
		}
		rel := edits.RelativeName(p, w.Root)
		if index.UnbuildableText(p, text) {
			unchecked = append(unchecked, rel+" (not part of any package the Go toolchain builds, so the type check cannot see it)")
			continue
		}
		uri, err := w.Client.EnsureOpen(p)
		if err == nil {
			var items []lsp.Diagnostic
			if items, err = w.Client.PullDiagnostics(ctx, uri); err == nil {
				entries = append(entries, diagdelta.FromLSP(rel, text, causes.explain(p, items))...)
				continue
			}
		}
		unchecked = append(unchecked, rel)
	}
	return entries, unchecked
}

// keepDiagnostics drops the noise from a diagnostics list: info and hints entirely, and warnings
// (gopls' analyzers, e.g. "unused parameter") in a directory where either side has an error, because
// gopls does not run analyzers in a package that fails to type-check, so they would flip in and out.
func keepDiagnostics(es []diagdelta.Entry, errDirs map[string]bool) []diagdelta.Entry {
	out := es[:0:0]
	for _, e := range es {
		if e.IsError() || (e.Severity == diagdelta.WarningSeverity && !errDirs[filepath.Dir(e.Path)]) {
			out = append(out, e)
		}
	}
	return out
}

// Delta compares diagnostics of `check` with `after` overlaid against `before` overlaid
// (nil = the disk as it is). skipBefore/skipAfter are files absent on that side.
func Delta(ctx context.Context, w *Workspace, before, after map[string]string, check []string, skipBefore, skipAfter map[string]bool) (diagdelta.Delta, error) {
	var be, ae []diagdelta.Entry
	var ub, ua []string
	cctx, cancel := context.WithTimeout(ctx, causeBudget)
	defer cancel()
	causes := &causeNamer{ctx: cctx, root: w.Root, known: map[string]string{}}
	run := func(overlay map[string]string, skip map[string]bool, out *[]diagdelta.Entry, unchecked *[]string) error {
		fn := func() error {
			*out, *unchecked = collect(ctx, w, overlay, check, skip, causes)
			return nil
		}
		if len(overlay) == 0 {
			return fn()
		}
		return w.Client.Overlay(ctx, overlay, fn)
	}
	if err := run(before, skipBefore, &be, &ub); err != nil {
		return diagdelta.Delta{}, err
	}
	if err := run(after, skipAfter, &ae, &ua); err != nil {
		return diagdelta.Delta{}, err
	}
	var ignored []string
	for _, side := range []*[]diagdelta.Entry{&be, &ae} {
		var files []string
		if *side, files = diagdelta.DropCgoArtefacts(*side); len(files) > 0 && side == &ae {
			ignored = append(ignored, fmt.Sprintf("cgo diagnostics in %s (a cgo package can't be loaded from an in-memory copy, so they say nothing about the edit; run go build to confirm)", strings.Join(files[:min(len(files), 4)], ", ")))
		}
	}
	errDirs := map[string]bool{}
	for _, es := range [][]diagdelta.Entry{be, ae} {
		for _, e := range es {
			if e.IsError() {
				errDirs[filepath.Dir(e.Path)] = true
			}
		}
	}
	d := diagdelta.Diff(keepDiagnostics(be, errDirs), keepDiagnostics(ae, errDirs))
	// Warnings of a directory with errors are left out of the comparison (analyzers do not run on a package
	// that fails to type-check); say so instead of silently reporting fewer warnings than `diagnostics` shows.
	hidden := func(es []diagdelta.Entry) []diagdelta.Entry {
		var out []diagdelta.Entry
		for _, e := range es {
			if e.Severity == diagdelta.WarningSeverity && errDirs[filepath.Dir(e.Path)] {
				out = append(out, e)
			}
		}
		return out
	}
	if n := len(diagdelta.Diff(hidden(be), hidden(ae)).New); n > 0 {
		ignored = append(ignored, fmt.Sprintf("%d new warning(s) in packages that have errors (analyzers such as vet do not run until they compile; `diagnostics` lists them)", n))
	}
	d.Ignored = ignored
	seen := map[string]bool{}
	_ = ub // A file only the before side could not check is checked after the edit, which is what the report is about
	for _, u := range ua {
		if !seen[u] {
			seen[u] = true
			d.Unchecked = append(d.Unchecked, u)
		}
	}
	d.FilesChecked = max(0, len(check)-len(d.Unchecked)) // a file that was "not checked" is not counted as checked
	return d, nil
}

// FilesToCheck: edited Go files first, then their package siblings and importers (capped). The
// second result lists the files left out by the cap. The third lists why the check is incomplete
// (reasons for NOT CHECKED): the importers could not be listed, or an edited package does not load.
func FilesToCheck(ctx context.Context, w *Workspace, edited []string, includeDependents bool) ([]string, []string, []string) {
	var goEdited []string
	for _, p := range edited {
		if strings.HasSuffix(p, ".go") {
			goEdited = append(goEdited, p)
		}
	}
	files := goEdited
	var incomplete []string
	if len(goEdited) > 0 {
		// The edited packages are loaded even without dependents: a package that does not load is not checked
		// whatever the importers are (include_dependents only decides whether the importers are type-checked).
		rel, problems, err := gomod.RelatedLoad(ctx, w.Root, goEdited)
		if includeDependents && len(rel) > 0 {
			files = rel
		}
		if err != nil {
			what := "the packages that import the edited files could not be listed"
			if !includeDependents {
				what = "the packages of the edited files could not be listed"
			}
			incomplete = append(incomplete, what+" ("+goErrText(err.Error(), w.Root)+")")
		}
		if len(problems) > 0 {
			shown := problems
			if len(shown) > 2 {
				shown = shown[:2]
			}
			var texts []string
			for _, p := range shown {
				texts = append(texts, goErrText(p, w.Root))
			}
			more := ""
			if len(problems) > len(shown) {
				more = fmt.Sprintf("; +%d more", len(problems)-len(shown))
			}
			incomplete = append(incomplete, "the package of an edited file does not load ("+strings.Join(texts, "; ")+more+")")
		}
	}
	var kept []string
	for _, f := range files {
		if !exclude.IsExcluded(f, w.Root) {
			kept = append(kept, f)
		}
	}
	if len(kept) > CheckLimit {
		return kept[:CheckLimit], kept[CheckLimit:], incomplete
	}
	return kept, nil, incomplete
}

// goErrText shows a go command's error with paths relative to the workspace, on one line.
func goErrText(msg, root string) string {
	msg = strings.ReplaceAll(msg, root+string(filepath.Separator), "")
	return strings.TrimSpace(strings.SplitN(msg, "\n", 2)[0])
}

// gateReasons lists why the type check of an edit is not complete (the NOT CHECKED reasons): the Go
// configuration does not load (health, "" when it does), the check could not cover what the edit touches
// (incomplete, from FilesToCheck), or no Go file was type-checked at all (checked == 0). An empty result means
// the check covered what the edit touches.
func gateReasons(health string, incomplete []string, checked int) []string {
	var out []string
	if health != "" {
		out = append(out, "the Go configuration does not load, so no package is loaded: "+health)
	}
	out = append(out, incomplete...)
	if checked == 0 {
		out = append(out, "no Go file was type-checked")
	}
	return out
}

// leftOutNote says exactly which files the CheckLimit cap skipped: the edited ones (the changed files
// themselves, which the answer must not present as dependents) and the dependent ones.
func leftOutNote(w *Workspace, left, edited []string) string {
	isEdited := map[string]bool{}
	for _, e := range edited {
		isEdited[e] = true
	}
	nEdited := 0
	var names []string
	for _, f := range left {
		if isEdited[f] {
			nEdited++
		}
	}
	for _, f := range left[:min(len(left), 6)] {
		names = append(names, edits.RelativeName(f, w.Root))
	}
	var parts []string
	if nEdited > 0 {
		parts = append(parts, fmt.Sprintf("%d edited file(s)", nEdited))
	}
	if nDep := len(left) - nEdited; nDep > 0 {
		parts = append(parts, fmt.Sprintf("%d dependent file(s)", nDep))
	}
	more := ""
	if len(left) > len(names) {
		more = fmt.Sprintf(", +%d more", len(left)-len(names))
	}
	return fmt.Sprintf("%s beyond the %d-file limit were not type-checked: %s%s", strings.Join(parts, " and "), CheckLimit, strings.Join(names, ", "), more)
}

// packageStub is what a deleted Go file leaves in the type-check: only its package clause, so the
// package still exists but the file's declarations are gone ("" when the clause cannot be read).
func packageStub(src string) string {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.PackageClauseOnly)
	if err != nil || f.Name == nil {
		return ""
	}
	return "package " + f.Name.Name + "\n"
}

// ignoreStub excludes a stub from every build. When every file of a directory is going away this
// makes an import of the package fail ("build constraints exclude all Go files") instead of
// resolving to an empty package.
func ignoreStub(stub string) string { return "//go:build ignore\n\n" + stub }

// dirFullyDeleted: the plan removes every Go file of dir and leaves none behind or adds one.
func dirFullyDeleted(dir string, plan *edits.EditPlan, deleted map[string]bool) bool {
	for _, c := range plan.Changes {
		if filepath.Dir(c.Path) == dir && c.Kind() != "delete" {
			return false
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !deleted[filepath.Join(dir, e.Name())] {
			return false
		}
	}
	return true
}

// Simulate shows what applying plan would do to the diagnostics, writing nothing.
func Simulate(ctx context.Context, w *Workspace, plan *edits.EditPlan, includeDependents bool) (diagdeltaResult, error) {
	after := map[string]string{}
	created, deleted := map[string]bool{}, map[string]bool{}
	var changed []string
	for _, c := range plan.Changes {
		if filepath.Ext(c.Path) != ".go" {
			continue
		}
		p := c.Path
		changed = append(changed, p)
		switch c.Kind() {
		case "delete":
			deleted[p] = true
			// gopls would otherwise keep loading the file from disk, so importers of what it
			// declared would still resolve.
			if c.Old != nil {
				if stub := packageStub(*c.Old); stub != "" {
					after[p] = stub
				}
			}
			continue
		case "create":
			created[p] = true
		}
		after[p] = *c.New
	}
	for p := range deleted {
		if stub := after[p]; stub != "" && dirFullyDeleted(filepath.Dir(p), plan, deleted) {
			after[p] = ignoreStub(stub)
		}
	}
	check, leftOut, incomplete := FilesToCheck(ctx, w, changed, includeDependents)
	d, err := Delta(ctx, w, nil, after, check, created, deleted)
	if err != nil {
		return d, err
	}
	for _, n := range nestedModuleNotes(ctx, w.Root, plan, planPaths(plan), check) {
		plan.Notes = appendUnique(plan.Notes, n)
	}
	if len(leftOut) > 0 {
		d.Unchecked = append(d.Unchecked, leftOutNote(w, leftOut, changed))
	}
	d.Capped = len(leftOut)
	d.DependentsOff = !includeDependents
	d.NotChecked = gateReasons(gomod.Health(ctx, w.Root), incomplete, d.FilesChecked)
	seenConstraint := map[string]bool{}
	for _, c := range plan.Changes {
		if filepath.Ext(c.Path) != ".go" {
			continue
		}
		// The file's own text: the stub standing in for a deleted file ("//go:build ignore") is ours, not its.
		src, ok := "", false
		switch {
		case c.New != nil:
			src, ok = *c.New, true
		case c.Old != nil:
			src, ok = *c.Old, true
		}
		if !ok || seenConstraint[src] {
			continue
		}
		// Only a file the build leaves out (by its name or its header, in the edit's text) gets the note.
		if reason, excluded := buildscope.ExcludedText(buildscope.Context(), c.Path, src); excluded {
			seenConstraint[src] = true // a moved file appears under its old and new path: one note
			plan.Notes = appendUnique(plan.Notes, excludedNote(edits.RelativeName(c.Path, w.Root), reason))
		}
	}
	return d, nil
}

// maxExcludedNames caps the names noteExcludedUses looks for in one edit. The scan reads the tree once, but every
// name is matched against each file that names it, so a large edit is cut short and the note says so.
const maxExcludedNames = 50

// topDeclShapes maps each top-level func (not a method), type, var and const name of src to its shape: the text of
// a func's signature (without its body), of a type or of a var or const without a type (the whole declaration), or
// of a typed var or const (its name and type). A grouped declaration counts as a whole, since an iota or an implicit
// repeat depends on the group.
func topDeclShapes(src string) map[string]string {
	out := map[string]string{}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil {
		return out
	}
	text := func(from, to token.Pos) string {
		return src[fset.Position(from).Offset:fset.Position(to).Offset]
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				end := d.End()
				if d.Body != nil {
					end = d.Body.Pos() // an edit to the body alone does not change the signature
				}
				out[d.Name.Name] = text(d.Pos(), end)
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					out[s.Name.Name] = text(d.Pos(), d.End())
				case *ast.ValueSpec:
					for _, id := range s.Names {
						if s.Type != nil {
							out[id.Name] = id.Name + " " + text(s.Type.Pos(), s.Type.End())
						} else {
							out[id.Name] = text(d.Pos(), d.End())
						}
					}
				}
			}
		}
	}
	return out
}

// noteExcludedUses adds a note for the top-level names that plan changes (added, removed, or given a new
// signature or type; an edit to a body alone changes none) and that a file the build leaves out still uses or
// declares again: gopls does not check those files, so the write gate cannot see the breakage. A note only: the
// write is not held. Files of the plan are not reported, nor a re-declaration in a file that cannot be built with
// the edited one (a _linux and a _windows file). The scan reads each declaring directory once and the tree once,
// for at most maxExcludedNames names; ctx cancels it.
func noteExcludedUses(ctx context.Context, w *Workspace, plan *edits.EditPlan) {
	texts, inPlan := map[string]string{}, map[string]bool{}
	for _, c := range plan.Changes {
		if filepath.Ext(c.Path) != ".go" {
			continue
		}
		inPlan[c.Path] = true
		if c.New != nil {
			texts[c.Path] = *c.New
		}
	}
	type nameKey struct{ dir, ip, name string }
	found := map[nameKey]string{}     // a changed name -> a file of its declaring package
	declared := map[string][]string{} // dir + "\x00" + name -> the constraint of each new declaration of it
	for _, c := range plan.Changes {
		if filepath.Ext(c.Path) != ".go" {
			continue
		}
		before, after := map[string]string{}, map[string]string{}
		if c.Old != nil {
			before = topDeclShapes(*c.Old)
		}
		if c.New != nil {
			after = topDeclShapes(*c.New)
		}
		dir := filepath.Dir(c.Path)
		ip := ""
		if m, err := moduleOf(dir); err == nil {
			ip, _ = importPathOf(m, dir)
		}
		names := map[string]bool{}
		for n := range before {
			names[n] = true
		}
		for n := range after {
			names[n] = true
		}
		for n := range names {
			if n == "_" || before[n] == after[n] {
				continue
			}
			k := nameKey{dir, ip, n}
			if _, ok := found[k]; !ok {
				found[k] = c.Path
			}
			if _, ok := after[n]; ok {
				dn := dir + "\x00" + n
				declared[dn] = append(declared[dn], buildscope.FileConstraint(c.Path, *c.New))
			}
		}
	}
	keys := make([]nameKey, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.dir != b.dir {
			return a.dir < b.dir
		}
		if a.ip != b.ip {
			return a.ip < b.ip
		}
		return a.name < b.name
	})
	total := len(keys)
	if total > maxExcludedNames {
		keys = keys[:maxExcludedNames]
	}
	var targets []buildscope.Target
	index := map[string]int{} // dir + "\x00" + import path -> its target
	for _, k := range keys {
		g := k.dir + "\x00" + k.ip
		i, ok := index[g]
		if !ok {
			i = len(targets)
			index[g] = i
			targets = append(targets, buildscope.Target{DeclFile: found[k], ImportPath: k.ip})
		}
		targets[i].Names = append(targets[i].Names, k.name)
	}
	res, err := buildscope.MentionsAll(ctx, buildscope.Context(), w.Root, texts, targets)
	if err != nil {
		plan.Notes = appendUnique(plan.Notes, "The check of uses in files the build leaves out was cancelled, so it may be incomplete.")
	}
	uses, decls := map[string][]string{}, map[string][]string{}
	for _, h := range res.Hits {
		if inPlan[h.Path] {
			continue
		}
		where := fmt.Sprintf("%s:%d (%s)", edits.RelativeName(h.Path, w.Root), h.Line, h.Reason)
		switch h.Kind {
		case buildscope.Use:
			uses[h.Name] = append(uses[h.Name], where)
		case buildscope.Decl:
			if declClashes(h.Path, declared[filepath.Dir(h.Path)+"\x00"+h.Name]) {
				decls[h.Name] = append(decls[h.Name], where)
			}
		}
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k.name] {
			continue
		}
		seen[k.name] = true
		name := k.name
		if len(uses[name]) > 0 {
			plan.Notes = appendUnique(plan.Notes, fmt.Sprintf("%d use(s) of %s in files the build leaves out were not checked: %s. Check them with a build that includes them (GOOS=... go build, or -tags in CODENAV_GO_MCP_BUILD_FLAGS).", len(uses[name]), name, listing(uses[name])))
		}
		if len(decls[name]) > 0 {
			plan.Notes = appendUnique(plan.Notes, fmt.Sprintf("%s is declared again in files the build leaves out: %s.", name, listing(decls[name])))
		}
	}
	if total > maxExcludedNames {
		plan.Notes = appendUnique(plan.Notes, fmt.Sprintf("Uses in files the build leaves out were checked for %d of the %d changed names only; check the others with a build that includes their files.", maxExcludedNames, total))
	}
}

// declClashes reports whether the file at path, which the build leaves out, can clash with one of the new
// declarations of a name whose constraints are cons: it cannot when no build has both files (a _linux and a
// _windows file). A removed name has no new declaration, so nothing clashes; a file that cannot be read clashes.
func declClashes(path string, cons []string) bool {
	if len(cons) == 0 {
		return false
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	mine := buildscope.FileConstraint(path, string(src))
	for _, c := range cons {
		if !buildscope.Exclusive(c, mine) {
			return true
		}
	}
	return false
}

type diagdeltaResult = diagdelta.Delta

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

var _ = lsp.PathToURI
