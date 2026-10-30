package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
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
	Apply             *bool  `json:"apply,omitempty"`
	MaxNewErrors      *int   `json:"max_new_errors,omitempty"`
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

func quickFix(ctx context.Context, w *Workspace, in quickFixIn) (string, error) {
	abs, err := w.absPath(in.FilePath)
	if err != nil {
		return "", err
	}
	text, _, err := edits.ReadSource(abs)
	if err != nil {
		return "", err
	}
	rel := edits.RelativeName(abs, w.Root)
	all := map[string][]lsp.TextEdit{}
	var titles []string

	if in.OrganizeImports && in.FixAll {
		return "", toolerr.Inputf("pass either organize_imports or fix_all, not both.")
	}
	var importSpecs []string
	if in.OrganizeImports || in.FixAll {
		kind, clean := "source.organizeImports", "The imports of "+rel+" are already organized."
		if in.FixAll {
			kind, clean = "source.fixAll", "gopls' fix-all has nothing to change in "+rel+"."
		}
		endLine, endCh := edits.PositionOf(text, len(text))
		acts, cmdOnly, err := actions(ctx, w, abs, lsp.Range{End: lsp.Position{Line: endLine, Character: endCh}}, []string{kind}, nil)
		if err != nil {
			return "", err
		}
		if len(acts) == 0 {
			if len(cmdOnly) > 0 {
				return fmt.Sprintf("gopls offers '%s' only as a command (not supported).", cmdOnly[0]), nil
			}
			return clean, nil
		}
		by, err := edits.ParseWorkspaceEdit(acts[0].Edit)
		if err != nil {
			return "", err
		}
		addEdits(all, by)
		titles = append(titles, tidy(acts[0].Title))
	} else {
		diags, err := w.Client.Diagnostics(ctx, abs)
		if err != nil {
			return "", err
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
			return "No diagnostics to fix in " + rel + at + ".", nil
		}
		var ambiguous, unfixable []string
		for i, d := range wanted {
			if i == 25 {
				break
			}
			label := fmt.Sprintf("line %d: %s", d.Range.Start.Line+1, strings.SplitN(d.Message, "\n", 2)[0])
			only := []string{"quickfix"}
			if strings.Trim(string(d.Code), `"`) == "unusedparams" {
				only = append(only, "refactor.rewrite.removeUnusedParam")
			}
			acts, cmdOnly, err := actions(ctx, w, abs, d.Range, only, []lsp.Diagnostic{d})
			if err != nil {
				return "", err
			}
			if in.Choice != "" {
				kept := acts[:0:0]
				for _, a := range acts {
					if strings.Contains(strings.ToLower(a.Title), strings.ToLower(in.Choice)) {
						kept = append(kept, a)
					}
				}
				acts = kept
			}
			switch {
			case len(acts) == 0:
				if len(cmdOnly) > 0 {
					label += fmt.Sprintf(" (gopls offers '%s' only as a command (not supported))", cmdOnly[0])
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
					return "", err
				}
				addEdits(all, by)
				titles = append(titles, tidy(acts[0].Title))
			default:
				opts := make([]string, 0, len(acts))
				for _, a := range acts[:min(len(acts), 8)] {
					opts = append(opts, "      - "+tidy(a.Title))
				}
				ambiguous = append(ambiguous, label+"\n"+strings.Join(opts, "\n"))
			}
		}
		if len(ambiguous) > 0 {
			return "Several fixes apply; nothing was changed. Repeat with choice=\"<part of a title>\":\n  " + strings.Join(ambiguous, "\n  "), nil
		}
		if len(all) == 0 && len(importSpecs) == 0 {
			hint := ""
			for _, u := range unfixable {
				if strings.Contains(u, "imported and not used") {
					hint = " For unused imports use organize_imports=true."
				}
			}
			return "gopls offers no quick fix for: " + strings.Join(unfixable[:min(len(unfixable), 6)], "; ") + "." + hint, nil
		}
	}

	changes, err := edits.ChangesFromEdits(all)
	if err != nil {
		return "", toolerr.Inputf("the fixes could not be combined: %v", err)
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
			return "", err
		}
		c, err := fileChange(abs, next, &old, bom)
		if err != nil {
			return "", err
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
	plan := edits.NewPlan("quick_fix "+rel+": "+strings.Join(uniq, "; "), changes)
	return finishPlan(ctx, w, plan, flag(in.Apply, true), limit(in.MaxNewErrors), flag(in.IncludeDependents, true), false)
}
