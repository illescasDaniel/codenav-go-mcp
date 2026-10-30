package tools

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/diagdelta"
	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// CheckLimit caps how many files are type-checked per simulation.
const CheckLimit = 60

// collect pulls diagnostics for paths (skipping `skip`), labelling them with the text in
// `overlays` when present, else the disk's. Call inside client.Overlay for the overlaid side.
func collect(ctx context.Context, w *Workspace, overlays map[string]string, paths []string, skip map[string]bool) ([]diagdelta.Entry, []string) {
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
		uri, err := w.Client.EnsureOpen(p)
		if err == nil {
			var items []lsp.Diagnostic
			if items, err = w.Client.PullDiagnostics(ctx, uri); err == nil {
				entries = append(entries, diagdelta.FromLSP(rel, text, items)...)
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
	run := func(overlay map[string]string, skip map[string]bool, out *[]diagdelta.Entry, unchecked *[]string) error {
		fn := func() error {
			*out, *unchecked = collect(ctx, w, overlay, check, skip)
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
	errDirs := map[string]bool{}
	for _, es := range [][]diagdelta.Entry{be, ae} {
		for _, e := range es {
			if e.IsError() {
				errDirs[filepath.Dir(e.Path)] = true
			}
		}
	}
	d := diagdelta.Diff(keepDiagnostics(be, errDirs), keepDiagnostics(ae, errDirs))
	d.FilesChecked = len(check)
	seen := map[string]bool{}
	for _, u := range append(ub, ua...) {
		if !seen[u] {
			seen[u] = true
			d.Unchecked = append(d.Unchecked, u)
		}
	}
	return d, nil
}

// FilesToCheck: edited Go files first, then their package siblings and importers (capped). The
// second result lists the files left out by the cap.
func FilesToCheck(ctx context.Context, w *Workspace, edited []string, includeDependents bool) ([]string, []string) {
	var goEdited []string
	for _, p := range edited {
		if strings.HasSuffix(p, ".go") {
			goEdited = append(goEdited, p)
		}
	}
	files := goEdited
	if includeDependents {
		if rel, _ := gomod.Related(ctx, w.Root, goEdited); len(rel) > 0 {
			files = rel
		}
	}
	var kept []string
	for _, f := range files {
		if !exclude.IsExcluded(f, w.Root) {
			kept = append(kept, f)
		}
	}
	if len(kept) > CheckLimit {
		return kept[:CheckLimit], kept[CheckLimit:]
	}
	return kept, nil
}

// leftOutNote says exactly which files the CheckLimit cap skipped.
func leftOutNote(w *Workspace, left []string) string {
	var names []string
	for _, f := range left[:min(len(left), 6)] {
		names = append(names, edits.RelativeName(f, w.Root))
	}
	more := ""
	if len(left) > len(names) {
		more = fmt.Sprintf(", +%d more", len(left)-len(names))
	}
	return fmt.Sprintf("%d dependent file(s) beyond the %d-file limit were not type-checked: %s%s", len(left), CheckLimit, strings.Join(names, ", "), more)
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
	check, leftOut := FilesToCheck(ctx, w, changed, includeDependents)
	d, err := Delta(ctx, w, nil, after, check, created, deleted)
	if err != nil {
		return d, err
	}
	if len(leftOut) > 0 {
		d.Unchecked = append(d.Unchecked, leftOutNote(w, leftOut))
	}
	for _, p := range changed {
		if b, err := os.ReadFile(p); err == nil && gomod.HasBuildConstraint(string(b)) {
			plan.Notes = appendUnique(plan.Notes, edits.RelativeName(p, w.Root)+" has a build constraint; diagnostics only cover the current GOOS/GOARCH/tags, so the check may not see this file's own errors.")
		}
	}
	return d, nil
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
