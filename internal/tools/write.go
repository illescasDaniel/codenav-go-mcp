package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/illescasDaniel/codenav-go-mcp/internal/diagdelta"
	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/format"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

// ReadOnlyEnv is named in the "writes disabled" message.
const ReadOnlyEnv = "CODENAV_GO_MCP_READ_ONLY"

// GoGuardRule is the Go-specific part of the write guard: only .go files, never generated ones.
func GoGuardRule(c edits.FileChange) error {
	if filepath.Ext(c.Path) != ".go" {
		return toolerr.Inputf("%s is not a .go file", filepath.Base(c.Path))
	}
	if c.Old != nil && gosrc.IsGenerated(*c.Old) {
		return toolerr.Inputf("%s is generated (// Code generated ... DO NOT EDIT.); change its generator instead", filepath.Base(c.Path))
	}
	return nil
}

// absPath resolves a workspace-relative path and checks the guard's path rules.
func (w *Workspace) absPath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(w.Root, p)
	}
	p = filepath.Clean(p)
	if err := w.Journal.Guard.CheckPath(p); err != nil {
		return "", err
	}
	return p, nil
}

func planPaths(p *edits.EditPlan) []string {
	var out []string
	for _, c := range p.Changes {
		out = append(out, c.Path)
	}
	return out
}

func maxErrors(n int) string {
	if n < 0 {
		return "no limit"
	}
	return fmt.Sprint(n)
}

// finishPlan simulates plan, then writes it when the new errors stay within maxNew (<0: unlimited)
// and apply is set; otherwise it keeps the plan as a preview.
func finishPlan(ctx context.Context, w *Workspace, plan *edits.EditPlan, apply bool, maxNew int, includeDeps, allowLarge bool) (string, error) {
	if len(plan.Changes) == 0 {
		return "No changes: the edit leaves the code as it is.", nil
	}
	if err := w.Journal.Guard.CheckPlan(plan, allowLarge || !apply); err != nil {
		return "", err
	}
	delta, err := Simulate(ctx, w, plan, includeDeps)
	if err != nil {
		return "", err
	}
	return conclude(w, plan, delta, apply, maxNew, allowLarge)
}

func conclude(w *Workspace, plan *edits.EditPlan, delta diagdelta.Delta, apply bool, maxNew int, allowLarge bool) (string, error) {
	var out []string
	out = append(out, plan.Description, plan.Summary(w.Root), "", diagdelta.Format(delta, 15))
	for _, n := range plan.Notes {
		out = append(out, "Note: "+n)
	}
	newErrs := len(delta.NewErrors())
	readOnly := w.Journal.Guard.CheckWritesEnabled(ReadOnlyEnv) != nil
	switch {
	case apply && (maxNew < 0 || newErrs <= maxNew):
		if err := w.Journal.Guard.CheckWritesEnabled(ReadOnlyEnv); err != nil {
			return strings.Join(append(out, "", "Not written: "+err.Error()), "\n"), nil
		}
		e, err := w.Journal.Apply(plan, allowLarge, ReadOnlyEnv)
		if err != nil {
			return "", err
		}
		_ = w.Client.NoteWritten(planPaths(plan))
		out = append(out, "", fmt.Sprintf("Applied (edit id %s); undo_edit reverts it.", e.ID))
	case apply && readOnly:
		out = append(out, "", fmt.Sprintf("Not written: %d new error(s) exceed max_new_errors=%s, and write tools are disabled (%s).", newErrs, maxErrors(maxNew), ReadOnlyEnv), "", plan.Diff(w.Root, 2, 120))
	case apply:
		id := w.Store.Add(plan)
		out = append(out, "", fmt.Sprintf("Not written: %d new error(s) exceed max_new_errors=%s. Preview id %s; call apply_edit(id=%q) to write it anyway.", newErrs, maxErrors(maxNew), id, id))
		out = append(out, "", plan.Diff(w.Root, 2, 120))
	case readOnly:
		out = append(out, "", "Preview only (apply=false); write tools are disabled ("+ReadOnlyEnv+").", "", plan.Diff(w.Root, 2, 200))
	default:
		id := w.Store.Add(plan)
		out = append(out, "", fmt.Sprintf("Preview only (apply=false). apply_edit(id=%q) writes it.", id), "", plan.Diff(w.Root, 2, 200))
	}
	return strings.Join(out, "\n"), nil
}

type editIn struct {
	FilePath          string  `json:"file_path" jsonschema:"Go file, relative to the workspace root"`
	OldString         string  `json:"old_string,omitempty"`
	NewString         *string `json:"new_string,omitempty" jsonschema:"replacement for old_string; pass \"\" to delete the text"`
	ReplaceAll        bool    `json:"replace_all,omitempty"`
	NewText           string  `json:"new_text,omitempty" jsonschema:"complete new file content (also creates a new file)"`
	Apply             *bool   `json:"apply,omitempty" jsonschema:"default true; false only previews"`
	MaxNewErrors      *int    `json:"max_new_errors,omitempty" jsonschema:"default 0; -1 = no limit"`
	IncludeDependents *bool   `json:"include_dependents,omitempty" jsonschema:"default true: also type-check importing packages"`
}

func flag(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func limit(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// addWrite registers a tool that may change files.
func addWrite[In any](s *mcp.Server, h Host, name, desc string, fn func(ctx context.Context, w *Workspace, in In) (string, error)) {
	t := &mcp.Tool{Name: name, Description: desc, Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), IdempotentHint: false}}
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		w, err := h.Acquire(ctx, req.Session)
		var out string
		if err == nil {
			out, err = fn(ctx, w, in)
		}
		if err != nil {
			t, ok := toolerr.Format(err)
			if !ok {
				return nil, nil, err
			}
			out = t
		}
		return text(h.Notes().Annotate(out)), nil, nil
	})
}

func boolPtr(b bool) *bool { return &b }

func planEdit(w *Workspace, in editIn) (*edits.EditPlan, error) {
	abs, err := w.absPath(in.FilePath)
	if err != nil {
		return nil, err
	}
	if filepath.Ext(abs) != ".go" {
		return nil, toolerr.Inputf("%q is not a .go file.", in.FilePath)
	}
	old, bom, rerr := edits.ReadSource(abs)
	exists := rerr == nil
	if rerr != nil && !os.IsNotExist(rerr) {
		return nil, rerr
	}
	var next string
	switch {
	case in.NewText != "":
		if in.OldString != "" {
			return nil, toolerr.Inputf("pass either old_string + new_string or the complete new_text, not both.")
		}
		next = in.NewText
	case in.OldString != "":
		if !exists {
			return nil, toolerr.Inputf("%s does not exist; pass new_text to create it.", in.FilePath)
		}
		if in.NewString == nil {
			return nil, toolerr.Inputf("old_string needs new_string; pass new_string=\"\" to delete the text.")
		}
		n := strings.Count(old, in.OldString)
		switch {
		case n == 0:
			return nil, toolerr.Inputf("old_string was not found in %s (it must match exactly, including whitespace).", in.FilePath)
		case n > 1 && !in.ReplaceAll:
			return nil, toolerr.Inputf("old_string matches %d times in %s; add context to make it unique or pass replace_all=true.", n, in.FilePath)
		}
		count := 1
		if in.ReplaceAll {
			count = -1
		}
		next = strings.Replace(old, in.OldString, edits.MatchEOL(*in.NewString, old), count)
	default:
		return nil, toolerr.Inputf("pass either old_string + new_string or the complete new_text.")
	}
	return planFile(abs, "edit "+in.FilePath, next, old, exists, bom)
}

// fileChange turns the complete new text of one file into a change: gofmt-ed, line endings and BOM
// of the old file kept. old is nil for a new file.
func fileChange(abs, next string, old *string, bom bool) (edits.FileChange, error) {
	formatted, err := gosrc.Format(abs, strings.ReplaceAll(next, "\r\n", "\n"))
	if err != nil {
		return edits.FileChange{}, err
	}
	c := edits.FileChange{Path: abs, NewBOM: bom}
	if old != nil {
		formatted = edits.MatchEOL(formatted, *old)
		c.Old, c.OldBOM = old, bom
	}
	c.New = &formatted
	return c, nil
}

// planFile is fileChange for a single-file plan. old/exists describe the file on disk.
func planFile(abs, desc, next, old string, exists, bom bool, notes ...string) (*edits.EditPlan, error) {
	var oldp *string
	if exists {
		oldp = &old
	}
	c, err := fileChange(abs, next, oldp, bom)
	if err != nil {
		return nil, err
	}
	if c.IsNoop() {
		return edits.NewPlan(desc, nil), nil
	}
	return edits.NewPlan(desc, []edits.FileChange{c}, notes...), nil
}

type applyIn struct {
	ID         string `json:"id" jsonschema:"preview id returned by a previous call"`
	AllowLarge bool   `json:"allow_large,omitempty"`
}

type undoIn struct {
	ID string `json:"id,omitempty" jsonschema:"edit id; default: the latest edit"`
}

type verifyIn struct {
	IncludeDependents *bool `json:"include_dependents,omitempty"`
}

// RegisterWrite registers the edit tools.
func RegisterWrite(s *mcp.Server, h Host) {
	addWrite(s, h, "edit", "Edit text in a Go file, type-checked: edit(file_path, old_string, new_string) or edit(file_path, new_text). "+
		"old_string must match exactly once unless replace_all=true. The result must parse; it is gofmt-ed. Writes when the new errors in the file and the packages importing it stay within max_new_errors (default 0); otherwise nothing is written and the result lists them plus an id for apply_edit. apply=false always previews; undo_edit reverts.",
		func(ctx context.Context, w *Workspace, in editIn) (string, error) {
			plan, err := planEdit(w, in)
			if err != nil {
				return "", err
			}
			return finishPlan(ctx, w, plan, flag(in.Apply, true), limit(in.MaxNewErrors), flag(in.IncludeDependents, true), false)
		})

	addWrite(s, h, "edit_symbol", "Replace, insert or delete a top-level func, method, type, const or var by name, type-checked like edit. "+
		"action=replace: name + source (the complete new definition, doc comment included; one not given is dropped and the result says so). "+
		"action=insert: source + file_path, position after/before a symbol (name), into a struct or interface (name), or end of the file (default; creates the file). "+
		"action=delete: name only; refuses while the symbol is used (force=true deletes anyway). imports=[...] adds the imports the new code needs; imports left unused by the edit are dropped. "+
		"Writes when the new errors stay within max_new_errors (default 0); apply=false previews; undo_edit reverts.",
		editSymbol)

	addWrite(s, h, "rename_symbol", "Rename a symbol everywhere it is used (gopls rename), type-checked. Give name (Square.Area, shapes.Scale; file_path narrows) or file_path+line+column (1-indexed). "+
		"linked=true (default): renaming a method also renames the interface methods it satisfies and every implementation, since gopls leaves those behind and the program would stop compiling. "+
		"The result lists the linked methods and the places still using the old name (strings, comments, docs). Writes when new errors stay within max_new_errors (default 0); apply=false previews; undo_edit reverts.",
		func(ctx context.Context, w *Workspace, in renameIn) (string, error) {
			plan, err := planRename(ctx, w, in)
			if err != nil {
				return "", err
			}
			return finishPlan(ctx, w, plan, flag(in.Apply, true), limit(in.MaxNewErrors), flag(in.IncludeDependents, true), in.AllowLarge)
		})

	addWrite(s, h, "quick_fix", "Apply gopls' quick fixes for a file's diagnostics (add a missing import, remove an unused variable or import, ...). Narrow with line or code. "+
		"Several different fixes for one diagnostic: nothing is applied, the options are listed; repeat with choice set to part of a title. organize_imports=true instead adds missing and removes unused imports, sorted. Type-checked like edit; undo_edit reverts.",
		quickFix)

	addWrite(s, h, "change_signature", "Add, remove or reorder a function's or method's parameters and update every call site. "+
		"name: Scale, shapes.Scale or Square.Area. add=[{name,type,value,position?}] (value is the argument written at every existing call: Go has no defaults); remove=[names]; reorder=[all remaining names in the new order]; imports=[...] for the added types/values. "+
		"Call sites come from gopls references. include_overrides (default true) changes an interface method and its implementations together. Uses as a value, f(g()) with several results and xs... to a changing variadic are listed as manual work. "+
		"Writes when new errors stay within max_new_errors (default 0); apply=false previews; undo_edit reverts.",
		changeSignature)

	addWrite(s, h, "move", "Move a top-level func, type, const or var to another file. Same directory: the declaration and the imports only it used move; "+
		"another directory (a package, created if missing): a type takes its methods along, package qualifiers are rewritten at every use (shapes.Square -> geom.Square), imports are fixed in every touched file. "+
		"Without name, file_path is a package directory and to_file its new directory: the .go files move (with the packages below) and every import of the old path is rewritten. "+
		"Refuses, with the reasons, when the moved code uses unexported identifiers that stay behind, when unexported members are used from outside, or when the move would create an import cycle. "+
		"Writes when new errors stay within max_new_errors (default 0); apply=false previews; undo_edit reverts.",
		moveSymbol)

	addWrite(s, h, "apply_edit", "Write a previewed edit by id, even if it adds errors. Refuses when a file changed since the preview.",
		func(ctx context.Context, w *Workspace, in applyIn) (string, error) {
			plan, err := w.Store.Get(in.ID)
			if err != nil {
				return "", err
			}
			if err := w.Journal.CheckFresh(plan); err != nil {
				return "", err
			}
			if err := w.Journal.Guard.CheckPlan(plan, in.AllowLarge); err != nil {
				return "", err
			}
			delta, err := Simulate(ctx, w, plan, true)
			if err != nil {
				return "", err
			}
			w.Store.Discard(in.ID)
			return conclude(w, plan, delta, true, -1, in.AllowLarge)
		})

	addWrite(s, h, "undo_edit", "Revert an applied edit (the latest by default). Refuses when its files changed afterwards.",
		func(ctx context.Context, w *Workspace, in undoIn) (string, error) {
			e, err := w.Journal.Undo(in.ID, ReadOnlyEnv)
			if err != nil {
				return "", err
			}
			_ = w.Client.NoteWritten(planPaths(e.Plan))
			return fmt.Sprintf("Reverted edit %s: %s\n%s", e.ID, e.Plan.Description, e.Plan.Summary(w.Root)), nil
		})

	addWrite(s, h, "verify_changes", "Diagnostics delta of the uncommitted changes against git HEAD: new vs fixed errors in changed Go files and the packages importing them.",
		func(ctx context.Context, w *Workspace, in verifyIn) (string, error) {
			return verifyChanges(ctx, w, flag(in.IncludeDependents, true))
		})
}

var _ = format.Relative
