package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"go/token"
	"regexp"
	"strconv"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
	"github.com/illescasDaniel/codenav-go-mcp/internal/index"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

type quickFixIn struct {
	FilePath          string `json:"file_path"`
	Line              int    `json:"line,omitempty" jsonschema:"1-indexed: only the diagnostics on this line"`
	Code              string `json:"code,omitempty" jsonschema:"only diagnostics with this code"`
	Choice            string `json:"choice,omitempty" jsonschema:"part of the title of the fix to use when several apply"`
	OrganizeImports   bool   `json:"organize_imports,omitempty" jsonschema:"instead of diagnostic fixes: add missing and remove unused imports, sorted"`
	FixAll            bool   `json:"fix_all,omitempty" jsonschema:"instead of diagnostic fixes: apply gopls' source.fixAll to the whole file"`
	Strict            *bool  `json:"strict,omitempty" jsonschema:"default false: apply nothing unless every targeted diagnostic gets a fix. With choice set the default is true when the choice fits only some diagnostics"`
	Apply             *bool  `json:"apply,omitempty"`
	MaxNewErrors      *int   `json:"max_new_errors,omitempty" jsonschema:"default 0; -1 = no limit; other negative values are refused"`
	IncludeDependents *bool  `json:"include_dependents,omitempty"`
}

type codeAction struct {
	Title string             `json:"title"`
	Kind  string             `json:"kind"`
	Edit  *lsp.WorkspaceEdit `json:"edit"`
	Data  json.RawMessage    `json:"data"`
	Cmd   json.RawMessage    `json:"command"`
	raw   json.RawMessage
}

// actions asks for the code actions in a range, resolving the ones that come without their edit.
// Actions that only run a server-side command (no edit, nothing to resolve) cannot be applied; their
// titles are returned apart.
func actions(ctx context.Context, w *Workspace, path string, rng lsp.Range, only []string, diags []lsp.Diagnostic) (out []codeAction, cmdOnly []string, err error) {
	raws, err := w.Client.CodeActions(ctx, path, rng, only, diags)
	if err != nil {
		return nil, nil, err
	}
	for _, raw := range raws {
		var a codeAction
		if json.Unmarshal(raw, &a) != nil {
			continue
		}
		a.raw = raw
		if a.Edit == nil && len(a.Data) > 0 {
			if res, rerr := w.Client.Request(ctx, "codeAction/resolve", raw); rerr == nil {
				var r codeAction
				if json.Unmarshal(res, &r) == nil && r.Edit != nil {
					a.Edit = r.Edit
				}
			}
		}
		switch {
		case a.Edit != nil:
			out = append(out, a)
		case len(a.Cmd) > 0:
			cmdOnly = append(cmdOnly, tidy(a.Title))
		}
	}
	return out, cmdOnly, nil
}

// addImportRE matches gopls' `Add import: "fmt"` and `Add import: str "strings"` titles.
var addImportRE = regexp.MustCompile(`^Add import:\s+(?:(\w+)\s+)?"([^"]+)"`)

func sameEdit(a, b codeAction) bool {
	x, _ := json.Marshal(a.Edit)
	y, _ := json.Marshal(b.Edit)
	return string(x) == string(y)
}

var spaceRunRE = regexp.MustCompile(` {2,}`)

// tidy collapses runs of spaces in a gopls action title for display (matching uses the original).
func tidy(title string) string { return spaceRunRE.ReplaceAllString(title, " ") }

// quickFixLimit caps how many diagnostics one quick_fix call examines.
const quickFixLimit = 25

// skippedDiagnostics says which diagnostics a quick_fix call did not look at ("" if it saw all).
func skippedDiagnostics(wanted []lsp.Diagnostic) string {
	if len(wanted) <= quickFixLimit {
		return ""
	}
	rest := wanted[quickFixLimit:]
	var lines []string
	for _, d := range rest[:min(len(rest), 8)] {
		lines = append(lines, fmt.Sprintf("line %d: %s", d.Range.Start.Line+1, strings.SplitN(d.Message, "\n", 2)[0]))
	}
	more := ""
	if len(rest) > len(lines) {
		more = fmt.Sprintf("; +%d more", len(rest)-len(lines))
	}
	return fmt.Sprintf("\nOnly the first %d of %d diagnostics were examined; not examined: %s%s. Repeat with line=N or code=... (or run again after applying) to reach them.", quickFixLimit, len(wanted), strings.Join(lines, "; "), more)
}

// isUnusedVar: gopls' (compiler) UnusedVar diagnostic, "declared and not used: x".
func isUnusedVar(d lsp.Diagnostic) bool {
	return strings.Trim(string(d.Code), `"`) == "UnusedVar" || strings.Contains(d.Message, "declared and not used")
}

// unusedName is the variable a "declared and not used" diagnostic names and its byte offset in text.
func unusedName(text string, d lsp.Diagnostic) (name string, start int, ok bool) {
	if d.Range.Start.Line != d.Range.End.Line {
		return "", 0, false
	}
	start, end := edits.Offset(text, d.Range.Start.Line, d.Range.Start.Character), edits.Offset(text, d.Range.End.Line, d.Range.End.Character)
	if end <= start || end > len(text) {
		return "", 0, false
	}
	name = text[start:end]
	if !token.IsIdentifier(name) || name == "_" {
		return "", 0, false
	}
	return name, start, true
}

// markUsedAction synthesizes the fix gopls does not offer for an unused local variable: `_ = x`
// after the declaring statement (see gosrc.MarkUsed).
func markUsedAction(abs, text string, d lsp.Diagnostic) (codeAction, bool) {
	name, start, ok := unusedName(text, d)
	if !ok {
		return codeAction{}, false
	}
	ins, err := gosrc.MarkUsed(text, start, name)
	if err != nil || len(ins) == 0 {
		return codeAction{}, false
	}
	tes := make([]lsp.TextEdit, len(ins))
	for i, in := range ins {
		l, c := edits.PositionOf(text, in.Offset)
		at := lsp.Position{Line: l, Character: c}
		tes[i] = lsp.TextEdit{Range: lsp.Range{Start: at, End: at}, NewText: in.Text}
	}
	return codeAction{
		Title: fmt.Sprintf("Mark %s as used (_ = %s)", name, name),
		Kind:  "quickfix",
		Edit:  &lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{lsp.PathToURI(abs): tes}},
	}, true
}

// removeUnusedAction is the second synthesized fix for an unused local variable: delete the declaration
// and keep whatever its initializer does (see gosrc.RemoveUnused). Parameters and results get none.
func removeUnusedAction(abs, text string, d lsp.Diagnostic) (codeAction, bool) {
	name, start, ok := unusedName(text, d)
	if !ok {
		return codeAction{}, false
	}
	rs, err := gosrc.RemoveUnused(text, start, name)
	if err != nil || len(rs) == 0 {
		return codeAction{}, false
	}
	tes := make([]lsp.TextEdit, len(rs))
	for i, r := range rs {
		sl, sc := edits.PositionOf(text, r.Start)
		el, ec := edits.PositionOf(text, r.End)
		tes[i] = lsp.TextEdit{Range: lsp.Range{Start: lsp.Position{Line: sl, Character: sc}, End: lsp.Position{Line: el, Character: ec}}, NewText: r.Text}
	}
	return codeAction{
		Title: "Remove unused variable " + name,
		Kind:  "quickfix",
		Edit:  &lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{lsp.PathToURI(abs): tes}},
	}, true
}

// qfResult is what one quick_fix run did: the text, the fixes written (or only previewed) and the
// diagnostics left alone with the reason.
type qfResult struct {
	out      string
	titles   []string
	notFixed []string
	wrote    bool
}

// quickFix runs quickFixRun and ends the answer with "Applied:" and "Not fixed:" lines.
func quickFix(ctx context.Context, w *Workspace, in quickFixIn) (string, error) {
	r, err := quickFixRun(ctx, w, in)
	if err != nil {
		return "", err
	}
	applied := "nothing"
	switch {
	case r.wrote && len(r.titles) > 0:
		applied = strings.Join(r.titles, "; ")
	case len(r.titles) > 0:
		applied = "nothing (not written: " + strings.Join(r.titles, "; ") + ")"
	}
	left := "none"
	if n := len(r.notFixed); n > 0 {
		left = strings.Join(r.notFixed[:min(n, 12)], "; ")
		if n > 12 {
			left += fmt.Sprintf("; +%d more", n-12)
		}
	}
	return r.out + "\nApplied: " + applied + "\nNot fixed: " + left, nil
}

func quickFixRun(ctx context.Context, w *Workspace, in quickFixIn) (qfResult, error) {
	abs, err := w.absPath(in.FilePath)
	if err != nil {
		return qfResult{}, err
	}
	text, _, err := edits.ReadSource(abs)
	if err != nil {
		return qfResult{}, err
	}
	rel := edits.RelativeName(abs, w.Root)
	if index.Unbuildable(abs) {
		return qfResult{}, notCompiledErr(rel)
	}
	all := map[string][]lsp.TextEdit{}
	var titles []string

	if in.OrganizeImports && in.FixAll {
		return qfResult{}, toolerr.Inputf("pass either organize_imports or fix_all, not both.")
	}
	var importSpecs []string
	var notes []string
	var notFixed []string
	if in.OrganizeImports || in.FixAll {
		kind, clean := "source.organizeImports", "The imports of "+rel+" are already organized."
		if in.FixAll {
			kind, clean = "source.fixAll", "gopls' fix-all has nothing to change in "+rel+"."
		}
		endLine, endCh := edits.PositionOf(text, len(text))
		acts, cmdOnly, err := actions(ctx, w, abs, lsp.Range{End: lsp.Position{Line: endLine, Character: endCh}}, []string{kind}, nil)
		if err != nil {
			return qfResult{}, err
		}
		if len(acts) == 0 {
			if len(cmdOnly) > 0 {
				return qfResult{out: fmt.Sprintf("gopls offers '%s' only as a command (not supported).", cmdOnly[0])}, nil
			}
			return qfResult{out: clean}, nil
		}
		by, err := edits.ParseWorkspaceEdit(acts[0].Edit)
		if err != nil {
			return qfResult{}, err
		}
		addEdits(all, by)
		titles = append(titles, tidy(acts[0].Title))
	} else {
		diags, err := w.Client.Diagnostics(ctx, abs)
		if err != nil {
			return qfResult{}, err
		}
		var wanted []lsp.Diagnostic
		for _, d := range diags {
			if in.Line > 0 && !(d.Range.Start.Line+1 <= in.Line && in.Line <= d.Range.End.Line+1) {
				continue
			}
			if in.Code != "" && strings.Trim(string(d.Code), `"`) != in.Code {
				continue
			}
			wanted = append(wanted, d)
		}
		if len(wanted) == 0 {
			at := ""
			if in.Line > 0 {
				at = fmt.Sprintf(" at line %d", in.Line)
			}
			return qfResult{out: "No diagnostics to fix in " + rel + at + "."}, nil
		}
		// reasons[i] says why wanted[i] is not fixed.
		reasons := map[int]string{}
		labels := make([]string, len(wanted))
		for i, d := range wanted {
			labels[i] = fmt.Sprintf("line %d: %s", d.Range.Start.Line+1, strings.SplitN(d.Message, "\n", 2)[0])
		}
		var ambiguous, unfixable, noMatch []string
		for i, d := range wanted {
			if i == quickFixLimit {
				break
			}
			label := labels[i]
			only := []string{"quickfix"}
			if strings.Trim(string(d.Code), `"`) == "unusedparams" {
				only = append(only, "refactor.rewrite.removeUnusedParam")
			}
			acts, cmdOnly, err := actions(ctx, w, abs, d.Range, only, []lsp.Diagnostic{d})
			if err != nil {
				return qfResult{}, err
			}
			if isUnusedVar(d) {
				if a, ok := markUsedAction(abs, text, d); ok {
					acts = append(acts, a)
				}
				if a, ok := removeUnusedAction(abs, text, d); ok {
					acts = append(acts, a)
				}
			}
			if in.Choice != "" {
				kept := acts[:0:0]
				for _, a := range acts {
					if strings.Contains(strings.ToLower(a.Title), strings.ToLower(in.Choice)) {
						kept = append(kept, a)
					}
				}
				if len(kept) == 0 && len(acts) > 0 {
					opts := make([]string, 0, len(acts))
					offered := make([]string, 0, len(acts))
					for _, a := range acts[:min(len(acts), 8)] {
						opts = append(opts, "      - "+tidy(a.Title))
						offered = append(offered, tidy(a.Title))
					}
					noMatch = append(noMatch, label+"\n"+strings.Join(opts, "\n"))
					reasons[i] = "choice matched none of: " + strings.Join(offered, " | ")
					continue
				}
				acts = kept
			}
			switch {
			case len(acts) == 0:
				reasons[i] = "no fix offered"
				if len(cmdOnly) > 0 {
					label += fmt.Sprintf(" (gopls offers '%s' only as a command (not supported))", cmdOnly[0])
					reasons[i] = "command-only: gopls offers '" + cmdOnly[0] + "' only as a command"
				}
				unfixable = append(unfixable, label)
			case func() bool {
				for _, o := range acts[1:] {
					if !sameEdit(acts[0], o) {
						return false
					}
				}
				return true
			}():
				if m := addImportRE.FindStringSubmatch(acts[0].Title); m != nil {
					importSpecs = append(importSpecs, strings.TrimSpace(m[1]+" "+strconv.Quote(m[2])))
					titles = append(titles, "Add import "+strconv.Quote(m[2]))
					continue
				}
				by, err := edits.ParseWorkspaceEdit(acts[0].Edit)
				if err != nil {
					return qfResult{}, err
				}
				if n := func() (n int) {
					for _, es := range by {
						n += len(es)
					}
					return
				}(); n == 0 {
					reasons[i] = "the fix offered (" + tidy(acts[0].Title) + ") changes nothing"
					continue
				}
				addEdits(all, by)
				titles = append(titles, tidy(acts[0].Title))
			default:
				opts := make([]string, 0, len(acts))
				names := make([]string, 0, len(acts))
				for _, a := range acts[:min(len(acts), 8)] {
					opts = append(opts, "      - "+tidy(a.Title))
					names = append(names, tidy(a.Title))
				}
				ambiguous = append(ambiguous, label+"\n"+strings.Join(opts, "\n"))
				reasons[i] = "ambiguous, several fixes apply: " + strings.Join(names, " | ")
			}
		}
		for i := quickFixLimit; i < len(wanted); i++ {
			reasons[i] = fmt.Sprintf("beyond the %d examined", quickFixLimit)
		}
		// left lists the diagnostics not fixed; hold names why a fixable one is among them when
		// nothing is applied.
		left := func(hold string) []string {
			var out []string
			for i := range wanted {
				switch r := reasons[i]; {
				case r != "":
					out = append(out, fmt.Sprintf("%s (reason: %s)", labels[i], r))
				case hold != "":
					out = append(out, fmt.Sprintf("%s (reason: %s)", labels[i], hold))
				}
			}
			return out
		}
		skipped := skippedDiagnostics(wanted)
		if len(ambiguous) > 0 {
			return qfResult{out: "Several fixes apply; nothing was changed. Repeat with choice=\"<part of a title>\":\n  " + strings.Join(ambiguous, "\n  ") + skipped,
				notFixed: left("not applied: other diagnostics need a choice")}, nil
		}
		if len(noMatch) > 0 && len(all) == 0 && len(importSpecs) == 0 {
			return qfResult{out: fmt.Sprintf("No fix matches choice=%q; nothing was changed. Offered:\n  %s%s", in.Choice, strings.Join(noMatch, "\n  "), skipped), notFixed: left("")}, nil
		}
		if len(all) == 0 && len(importSpecs) == 0 {
			hint := ""
			for _, u := range unfixable {
				if strings.Contains(u, "imported and not used") {
					hint = " For unused imports use organize_imports=true."
				}
			}
			if len(unfixable) == 0 {
				// The fixes gopls offered changed nothing (e.g. an import that does not resolve).
				for i, d := range wanted {
					if i == 6 {
						break
					}
					unfixable = append(unfixable, fmt.Sprintf("line %d: %s", d.Range.Start.Line+1, strings.SplitN(d.Message, "\n", 2)[0]))
				}
				return qfResult{out: fmt.Sprintf("gopls offers no quick fix that changes anything for the %d diagnostic(s): %s.%s%s", len(wanted), strings.Join(unfixable, "; "), hint, skipped),
					notFixed: left("the fix offered changes nothing")}, nil
			}
			return qfResult{out: "gopls offers no quick fix for: " + strings.Join(unfixable[:min(len(unfixable), 6)], "; ") + "." + hint + skipped, notFixed: left("")}, nil
		}
		// A strict run applies all or nothing; a choice that fits only some diagnostics is usually aimed
		// at one fix, so it is strict unless asked otherwise.
		strict, implicit := flag(in.Strict, false), false
		if in.Strict == nil && in.Choice != "" && len(noMatch) > 0 {
			strict, implicit = true, true
		}
		if rest := left(""); strict && len(rest) > 0 {
			why := fmt.Sprintf("strict=true: %d targeted diagnostic(s) are not fixed, so nothing was applied.", len(rest))
			if implicit {
				why = fmt.Sprintf("choice=%q fits only some of the diagnostics, so nothing was applied (a choice is usually aimed at one fix; pass strict=false to apply the matching ones).", in.Choice)
			}
			hold := "its fix was not applied: strict=true and other targeted diagnostics are not fixed"
			if implicit {
				hold = fmt.Sprintf("its fix matches choice=%q but was held back: the choice fits only some of the diagnostics (strict=false applies the matching ones)", in.Choice)
			}
			return qfResult{out: why + skipped, notFixed: left(hold)}, nil
		}
		notFixed = left("")
		if skipped != "" {
			notes = append(notes, strings.TrimSpace(skipped))
		}
	}

	changes, err := edits.ChangesFromEdits(all)
	if err != nil {
		return qfResult{}, toolerr.Inputf("the fixes could not be combined: %v", err)
	}
	for i, c := range changes {
		// A file that was gofmt-clean stays so (a removal can leave blanks behind); a messy one keeps its text.
		if c.Old != nil && c.New != nil && strings.HasSuffix(c.Path, ".go") {
			next := gosrc.DropOrphanImportComments(c.Path, *c.Old, *c.New)
			if f, ferr := fileChange(c.Path, next, c.Old, c.OldBOM); ferr == nil {
				f.NewBOM = c.NewBOM
				changes[i] = f
			}
		}
	}
	if len(importSpecs) > 0 {
		// Several "Add import" fixes are merged into one change so a single import block results.
		base, old, bom := text, text, false
		idx := -1
		for i, c := range changes {
			if c.Path == abs {
				idx = i
				base = *c.New
			}
		}
		_, bom, _ = edits.ReadSource(abs)
		next, err := gosrc.EditImports(abs, base, "", importSpecs, false)
		if err != nil {
			return qfResult{}, err
		}
		c, err := fileChange(abs, next, &old, bom)
		if err != nil {
			return qfResult{}, err
		}
		switch {
		case idx >= 0:
			changes[idx] = c
		case !c.IsNoop():
			changes = append(changes, c)
		}
	}
	seen := map[string]bool{}
	var uniq []string
	for _, t := range titles {
		if !seen[t] {
			seen[t] = true
			uniq = append(uniq, t)
		}
	}
	plan := edits.NewPlan("quick_fix "+rel+": "+strings.Join(uniq, "; "), changes, notes...)
	out, id, err := finishPlanID(ctx, w, plan, flag(in.Apply, true), limit(in.MaxNewErrors), flag(in.IncludeDependents, true), false)
	return qfResult{out: out, titles: uniq, notFixed: notFixed, wrote: id != ""}, err
}
