package tools

import (
	"context"
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

func dropHints(es []diagdelta.Entry) []diagdelta.Entry {
	out := es[:0:0]
	for _, e := range es {
		if e.Severity != diagdelta.HintSeverity {
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
	d := diagdelta.Diff(dropHints(be), dropHints(ae))
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

// FilesToCheck: edited Go files first, then their package siblings and importers (capped).
func FilesToCheck(ctx context.Context, w *Workspace, edited []string, includeDependents bool) ([]string, int) {
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
		return kept[:CheckLimit], len(kept) - CheckLimit
	}
	return kept, 0
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
			continue
		case "create":
			created[p] = true
		}
		after[p] = *c.New
	}
	check, leftOut := FilesToCheck(ctx, w, changed, includeDependents)
	d, err := Delta(ctx, w, nil, after, check, created, deleted)
	if err != nil {
		return d, err
	}
	if leftOut > 0 {
		d.Unchecked = append(d.Unchecked, "more dependent file(s) beyond the 60-file limit")
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
