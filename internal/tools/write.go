package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/illescasDaniel/codenav-go-mcp/internal/diagdelta"
	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
	"github.com/illescasDaniel/codenav-go-mcp/internal/format"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
	"github.com/illescasDaniel/codenav-go-mcp/internal/txn"
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

// argPath is absPath for a path the caller wrote (file_path, to_file): one that lies outside the workspace is
// refused in terms of what was asked, as the file the tool was pointed at, not as an effect of the edit.
func (w *Workspace) argPath(p string) (string, error) {
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(w.Root, abs)
	}
	if rel, err := filepath.Rel(w.Root, filepath.Clean(abs)); err == nil && (rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return "", toolerr.Inputf("%s is outside the workspace; the write tools only change files inside it.", p)
	}
	return w.absPath(p)
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
	out, _, err := finishPlanID(ctx, w, plan, apply, maxNew, includeDeps, allowLarge)
	return out, err
}

// finishPlanID is finishPlan that also returns the id of the edit when it was written ("" otherwise).
func finishPlanID(ctx context.Context, w *Workspace, plan *edits.EditPlan, apply bool, maxNew int, includeDeps, allowLarge bool) (string, string, error) {
	if len(plan.Changes) == 0 {
		return "No changes: the edit leaves the code as it is.", "", nil
	}
	if err := w.Journal.Guard.CheckPlan(plan, allowLarge || !apply); err != nil {
		return "", "", err
	}
	delta, err := Simulate(ctx, w, plan, includeDeps)
	if err != nil {
		return "", "", err
	}
	plan.Notes = append(plan.Notes, unformattedNotes(w.Root, plan)...)
	for _, c := range plan.Changes {
		plan.Notes = append(plan.Notes, c.Notes...)
	}
	return conclude(w, plan, delta, apply, maxNew, allowLarge)
}

// unformattedNotes names the modified files that were not gofmt-clean before the edit and are not now:
// they were left as they were so the diff shows the edit only.
func unformattedNotes(root string, plan *edits.EditPlan) []string {
	var names []string
	for _, c := range plan.Changes {
		if c.Old != nil && c.New != nil && strings.HasSuffix(c.Path, ".go") && !isGofmted(c.Path, *c.Old) {
			names = append(names, edits.RelativeName(c.Path, root))
		}
	}
	if len(names) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("not gofmt-clean before the edit, so not reformatted (the diff shows the edit only; run gofmt -w to format): %s", strings.Join(names[:min(len(names), 5)], ", "))}
}

// nestedModuleNotes warns when edited files belong to a module the workspace does not build (a
// nested go.mod with no go.work listing it): the edit rewrites only workspace code, so callers in other
// modules are type-checked (their errors are listed) but never edited.
//
// checked lists the files the simulation type-checked: the note is only worth showing when one of them
// lies outside the module and uses a declaration the edit changes (a real caller in a sibling module) -
// otherwise nothing is left unedited.
func nestedModuleNotes(ctx context.Context, root string, plan *edits.EditPlan, paths, checked []string) []string {
	seen := map[string]bool{}
	var notes []string
	for _, p := range paths {
		n, ok := gomod.Uncovered(root, p)
		if !ok || seen[n.Dir] {
			continue
		}
		seen[n.Dir] = true
		// Only what a caller can depend on matters: the declarations the edit adds, removes or changes (a
		// function body is not one). With those known, a sibling module counts only when its files use one.
		changed, changedObjs, known := changedDecls(plan, n.Dir)
		// By name first (cheap), then by type: a file that merely has a method or function of the same name
		// as a changed one, or whose package never refers to the edited one, uses nothing the edit changes.
		wanted := map[string]bool{}
		for dir, keys := range changedObjs {
			if rel, err := filepath.Rel(n.Dir, dir); err == nil && n.Path != "" {
				pkg := n.Path
				if rel != "." {
					pkg += "/" + filepath.ToSlash(rel)
				}
				for k := range keys {
					wanted[pkg+"|"+k] = true
				}
			}
		}
		typed := map[string]map[string]map[string]bool{} // directory -> file -> objects used
		outside := false
		for _, f := range checked {
			if rel, err := filepath.Rel(n.Dir, f); err != nil || strings.HasPrefix(rel, "..") {
				if known && !mentionsAny(f, changed) {
					continue
				}
				if known && n.Path != "" {
					dir := filepath.Dir(f)
					if _, done := typed[dir]; !done {
						typed[dir] = usedObjects(ctx, dir)
					}
					if used, ok := typed[dir][f]; ok {
						hit := false
						for k := range used {
							if wanted[k] {
								hit = true
								break
							}
						}
						if !hit {
							continue
						}
					}
				}
				outside = true
				break
			}
		}
		if !outside {
			continue
		}
		name := n.Path
		if name == "" {
			name = edits.RelativeName(n.Dir, root)
		}
		dirs := []string{"./" + edits.RelativeName(n.Dir, root)}
		for _, o := range gomod.UncoveredModules(root, exclude.IsDirName, 6) {
			if o.Dir != n.Dir {
				dirs = append(dirs, "./"+edits.RelativeName(o.Dir, root))
			}
		}
		notes = append(notes, fmt.Sprintf("%s belongs to module %s (%s/go.mod), which no go.work lists, so only this module is edited: callers in other modules (a replace directive, a sibling directory) are type-checked, and any breakage shows above as a new error, but they are NOT rewritten - fix them by hand, or join the modules with a go.work (go work init %s) so the edit rewrites them too.",
			edits.RelativeName(p, root), name, edits.RelativeName(n.Dir, root), strings.Join(dirs, " ")))
	}
	return notes
}

func conclude(w *Workspace, plan *edits.EditPlan, delta diagdelta.Delta, apply bool, maxNew int, allowLarge bool) (string, string, error) {
	var out []string
	editID := ""
	out = append(out, plan.Description, plan.Summary(w.Root), "", diagdelta.Format(delta, 15))
	for _, n := range plan.Notes {
		out = append(out, "Note: "+n)
	}
	newErrs := len(delta.NewErrors())
	readOnly := w.Journal.Guard.CheckWritesEnabled(ReadOnlyEnv) != nil
	refused := false // a write that was not made: the answer is a refusal (isError), the text still shows the diff
	switch {
	case apply && len(plan.Holds) > 0 && !readOnly:
		// A check the gate cannot make (code the build leaves out): held as a preview, like new errors.
		refused = true
		plan.AllowLarge = allowLarge
		id := w.Store.Add(plan)
		out = append(out, "", "Not written: "+strings.Join(plan.Holds, " ")+fmt.Sprintf(" Preview id %s; call apply_edit(id=%q%s) to write it anyway.", id, id, largeArg(w, plan)))
		out = append(out, "", plan.Diff(w.Root, 2, 120))
	case apply && (maxNew < 0 || newErrs <= maxNew):
		if err := w.Journal.Guard.CheckWritesEnabled(ReadOnlyEnv); err != nil {
			// The diff still goes with the refusal: a read-only server exists to show what would change.
			refused = true
			out = append(out, "", "Not written: "+err.Error(), "", plan.Diff(w.Root, 2, 200))
			break
		}
		e, err := w.Journal.Apply(plan, allowLarge, ReadOnlyEnv)
		if err != nil {
			return "", "", err
		}
		editID = e.ID
		_ = w.Client.NoteWritten(planPaths(plan))
		out = append(out, "", fmt.Sprintf("Applied (edit id %s); undo_edit reverts it.", e.ID))
	case apply && readOnly:
		refused = true
		out = append(out, "", fmt.Sprintf("Not written: %d new error(s) exceed max_new_errors=%s, and write tools are disabled (%s).", newErrs, maxErrors(maxNew), ReadOnlyEnv), "", plan.Diff(w.Root, 2, 120))
	case apply:
		refused = true
		plan.AllowLarge = allowLarge
		id := w.Store.Add(plan)
		out = append(out, "", fmt.Sprintf("Not written: %d new error(s) exceed max_new_errors=%s. Preview id %s; call apply_edit(id=%q%s) to write it anyway.", newErrs, maxErrors(maxNew), id, id, largeArg(w, plan)))
		out = append(out, "", plan.Diff(w.Root, 2, 120))
	case readOnly:
		out = append(out, "", "Preview only (apply=false); write tools are disabled ("+ReadOnlyEnv+").")
		if maxNew >= 0 && newErrs > maxNew {
			out = append(out, fmt.Sprintf("Warning: with apply=true this edit would be refused, because its %d new error(s) exceed max_new_errors=%s; it would apply with max_new_errors=%d (or -1 for no limit) - if write tools were enabled.", newErrs, maxErrors(maxNew), newErrs))
		}
		out = append(out, "", plan.Diff(w.Root, 2, 200))
	default:
		plan.AllowLarge = allowLarge
		id := w.Store.Add(plan)
		out = append(out, "", fmt.Sprintf("Preview only (apply=false). apply_edit(id=%q%s) writes it.", id, largeArg(w, plan)))
		if len(plan.Holds) > 0 {
			out = append(out, "Warning: with apply=true this edit is not written: "+strings.Join(plan.Holds, " "))
		}
		if maxNew >= 0 && newErrs > maxNew {
			out = append(out, fmt.Sprintf("Warning: with apply=true this edit would be refused, because its %d new error(s) exceed max_new_errors=%s; it would apply with max_new_errors=%d (or -1 for no limit). apply_edit(id=%q) writes it regardless.", newErrs, maxErrors(maxNew), newErrs, id))
		}
		out = append(out, "", plan.Diff(w.Root, 2, 200))
	}
	answer := strings.Join(out, "\n")
	if refused {
		// The text is the answer and the error carries it too (a refusal is isError; the caller keeps the text).
		return answer, "", toolerr.Inputf("%s", answer)
	}
	return answer, editID, nil
}

// largeArg is the extra apply_edit argument a preview over the file limit needs: none when
// allow_large was given with the preview (the stored plan remembers it).
func largeArg(w *Workspace, plan *edits.EditPlan) string {
	limit := w.Journal.Guard.MaxFiles
	if limit == 0 {
		limit = txn.DefaultMaxFiles
	}
	if plan.AllowLarge || len(plan.Changes) <= limit {
		return ""
	}
	return fmt.Sprintf(", allow_large=true; it touches %d files, over the limit of %d", len(plan.Changes), limit)
}

type editIn struct {
	FilePath          string  `json:"file_path" jsonschema:"Go file, relative to the workspace root"`
	OldString         string  `json:"old_string,omitempty"`
	NewString         *string `json:"new_string,omitempty" jsonschema:"replacement for old_string; pass \"\" to delete the text"`
	ReplaceAll        bool    `json:"replace_all,omitempty"`
	NewText           string  `json:"new_text,omitempty" jsonschema:"complete new file content (also creates a new file)"`
	Apply             *bool   `json:"apply,omitempty" jsonschema:"default true; false only previews"`
	MaxNewErrors      *int    `json:"max_new_errors,omitempty" jsonschema:"default 0; -1 = no limit; other negative values are refused"`
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

var (
	applyIDRE  = regexp.MustCompile(` plus an id for apply_edit`)
	undoTailRE = regexp.MustCompile(`;? undo_edit reverts\.`)
)

// readOnlyDesc rewrites the tail of a tool description for a read-only server, which has neither
// apply_edit nor undo_edit.
func readOnlyDesc(desc string, readOnly bool) string {
	if !readOnly {
		return desc
	}
	desc = applyIDRE.ReplaceAllString(desc, "")
	return undoTailRE.ReplaceAllString(desc, " (read-only server: previews only).")
}

// addWrite registers a tool that may change files.
func addWrite[In any](s *mcp.Server, h Host, readOnly bool, name, desc string, fn func(ctx context.Context, w *Workspace, in In) (string, error)) {
	registerUsage[In](name)
	t := &mcp.Tool{Name: name, Description: readOnlyDesc(desc, readOnly), Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(false), IdempotentHint: false}}
	if readOnly {
		// `apply` stays in the schema (clients that always send it must keep working): apply=false previews,
		// apply=true is refused with the diff.
		if sch, err := jsonschema.For[In](nil); err == nil && sch.Properties["apply"] != nil {
			sch.Properties["apply"].Description = "this server is read-only: apply=false previews, apply=true is refused (the preview is shown with it)"
			t.InputSchema = sch
		}
	}
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		w, err := h.Acquire(ctx, req.Session)
		var out string
		if err == nil {
			if err = checkMaxNewErrors(in); err == nil {
				out, err = fn(ctx, w, in)
			}
		}
		res, rerr := finishResult(ctx, h, w, true, out, err)
		return res, nil, rerr
	})
}

// checkMaxNewErrors refuses a max_new_errors below -1 (-1 means no limit; other negatives are
// a mistake that would silently turn the safety check off).
func checkMaxNewErrors(in any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return nil
	}
	var f struct {
		Max *int `json:"max_new_errors"`
	}
	if json.Unmarshal(b, &f) == nil && f.Max != nil && *f.Max < -1 {
		return toolerr.Argf("max_new_errors=%d is invalid: use a count >= 0, or -1 for no limit.", *f.Max)
	}
	return nil
}

func boolPtr(b bool) *bool { return &b }

func planEdit(w *Workspace, in editIn) (*edits.EditPlan, error) {
	abs, err := w.argPath(in.FilePath)
	if err != nil {
		return nil, err
	}
	if filepath.Ext(abs) != ".go" {
		return nil, toolerr.Argf("%q is not a .go file.", in.FilePath)
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
			return nil, toolerr.Argf("pass either old_string + new_string or the complete new_text, not both.")
		}
		next = in.NewText
	case in.OldString != "":
		if !exists {
			return nil, toolerr.Argf("%s does not exist; pass new_text to create it.", in.FilePath)
		}
		if in.NewString == nil {
			return nil, toolerr.Argf("old_string needs new_string; pass new_string=\"\" to delete the text.")
		}
		// Match on LF-normalised text, so an old_string written with LF finds CRLF (or mixed) lines;
		// fileChange gives the result the file's line endings back, line by line.
		hay, needle := strings.ReplaceAll(old, "\r\n", "\n"), strings.ReplaceAll(in.OldString, "\r\n", "\n")
		n := strings.Count(hay, needle)
		switch {
		case n == 0:
			return nil, toolerr.Argf("old_string was not found in %s (it must match exactly, including whitespace).", in.FilePath)
		case n > 1 && !in.ReplaceAll:
			return nil, toolerr.Argf("old_string matches %d times in %s; add context to make it unique or pass replace_all=true.", n, in.FilePath)
		}
		count := 1
		if in.ReplaceAll {
			count = -1
		}
		next = strings.Replace(hay, needle, strings.ReplaceAll(*in.NewString, "\r\n", "\n"), count)
	default:
		return nil, toolerr.Inputf("pass either old_string + new_string or the complete new_text.")
	}
	return planFile(abs, "edit "+in.FilePath, next, old, exists, bom)
}

// pkgOfFile is the package a file belongs to: its package clause, minus the _test suffix of an external
// test package (cs_test in cs_test.go is cs's own test package).
func pkgOfFile(path, src string) string {
	name := gosrc.PackageName(src)
	if strings.HasSuffix(path, "_test.go") {
		name = strings.TrimSuffix(name, "_test")
	}
	return name
}

// packageClauseRefusal refuses, with one message, an edit that gives a file of an existing package a
// package clause that differs from the other files of its directory: the type checker would report the
// same cause twice and the package would stop building. maxNew < 0 or >= 2 means the caller accepts the
// errors (renaming the package file by file).
func packageClauseRefusal(w *Workspace, plan *edits.EditPlan, maxNew int) error {
	if maxNew < 0 || maxNew >= 2 {
		return nil
	}
	inPlan := map[string]bool{}
	for _, c := range plan.Changes {
		inPlan[c.Path] = true
	}
	for _, c := range plan.Changes {
		if c.New == nil || filepath.Ext(c.Path) != ".go" {
			continue
		}
		next := pkgOfFile(c.Path, *c.New)
		if next == "" {
			continue
		}
		if c.Old != nil && pkgOfFile(c.Path, *c.Old) == next {
			continue // the package clause is not what changes
		}
		ents, err := os.ReadDir(filepath.Dir(c.Path))
		if err != nil {
			continue
		}
		others := map[string]map[string][]string{} // package -> clause as written -> files
		for _, e := range ents {
			p := filepath.Join(filepath.Dir(c.Path), e.Name())
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || inPlan[p] {
				continue
			}
			src, _, rerr := edits.ReadSource(p)
			if rerr != nil || strings.Contains(src[:min(len(src), 400)], "go:build ignore") {
				continue
			}
			if n := pkgOfFile(p, src); n != "" {
				if others[n] == nil {
					others[n] = map[string][]string{}
				}
				written := gosrc.PackageName(src)
				others[n][written] = append(others[n][written], e.Name())
			}
		}
		if len(others) != 1 {
			continue // a new directory, or one that already mixes packages
		}
		for name, clauses := range others {
			if name == next {
				continue
			}
			rel := edits.RelativeName(c.Path, w.Root)
			// Each clause with the files that really declare it (an external test package is another
			// clause of the same package), the plain one first.
			var written []string
			for clause := range clauses {
				written = append(written, clause)
			}
			sort.Slice(written, func(i, j int) bool {
				if (written[i] == name) != (written[j] == name) {
					return written[i] == name
				}
				return written[i] < written[j]
			})
			var groups []string
			for _, clause := range written {
				files := clauses[clause]
				sort.Strings(files)
				more := ""
				if len(files) > 3 {
					more = fmt.Sprintf(", +%d more", len(files)-3)
				}
				groups = append(groups, fmt.Sprintf("`package %s` (%s%s)", clause, strings.Join(files[:min(len(files), 3)], ", "), more))
			}
			return toolerr.Inputf("%s would declare `package %s`, but the other files of %s declare %s: one directory is one package. Keep `package %s`, or change the package clause of all its files; to do that file by file pass max_new_errors=-1.",
				rel, gosrc.PackageName(*c.New), edits.RelativeName(filepath.Dir(c.Path), w.Root), strings.Join(groups, " and "), name)
		}
	}
	return nil
}

// isGofmted reports whether text (any line endings) is what gofmt would leave unchanged. (goimports'
// merging of two import declarations does not count: a file gofmt accepts is clean.)
func isGofmted(abs, text string) bool {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return gosrc.GofmtClean(text)
}

// fileChange turns the complete new text of one file into a change: gofmt-ed, line endings and BOM
// of the old file kept. old is nil for a new file. An existing file that was not gofmt-clean before the
// edit is not reformatted (the diff would be full of unrelated changes); it still has to parse, and
// unformattedNotes says so.
func fileChange(abs, next string, old *string, bom bool) (edits.FileChange, error) {
	formatted, err := gosrc.Format(abs, strings.ReplaceAll(next, "\r\n", "\n"))
	if err != nil {
		return edits.FileChange{}, err
	}
	if old != nil && !isGofmted(abs, *old) {
		formatted = strings.ReplaceAll(next, "\r\n", "\n")
	}
	c := edits.FileChange{Path: abs, NewBOM: bom}
	if strings.HasSuffix(abs, ".go") {
		was := map[string]bool{}
		if old != nil {
			for _, d := range gosrc.DuplicateImports(*old) {
				was[d] = true
			}
		}
		for _, d := range gosrc.DuplicateImports(next) {
			if !was[d] {
				c.Notes = append(c.Notes, fmt.Sprintf("%s listed %s twice; formatting merged them into one", filepath.Base(abs), d))
			}
		}
	}
	if old != nil {
		formatted = edits.RestoreEOL(formatted, *old)
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
// With readOnly, apply_edit and undo_edit are not offered: nothing can be written or reverted.
func RegisterWrite(s *mcp.Server, h Host, readOnly bool) {
	addWrite(s, h, readOnly, "edit", "Edit text in a Go file, type-checked: edit(file_path, old_string, new_string) or edit(file_path, new_text). "+
		"old_string must match exactly once unless replace_all=true. The result must parse; it is gofmt-ed. Writes when the new errors in the file and the packages importing it stay within max_new_errors (default 0); otherwise nothing is written and the result lists them plus an id for apply_edit. apply=false always previews; undo_edit reverts.",
		func(ctx context.Context, w *Workspace, in editIn) (string, error) {
			plan, err := planEdit(w, in)
			if err != nil {
				return "", err
			}
			if err := packageClauseRefusal(w, plan, limit(in.MaxNewErrors)); err != nil {
				return "", err
			}
			return finishPlan(ctx, w, plan, flag(in.Apply, true), limit(in.MaxNewErrors), flag(in.IncludeDependents, true), false)
		})

	addWrite(s, h, readOnly, "edit_symbol", "Replace, insert or delete a top-level func, method, type, const or var by name, type-checked like edit. "+
		"action=replace: name + source (the complete new definition, doc comment included; one not given is dropped and the result says so). "+
		"action=insert: source + file_path, position after/before a symbol (name), into a struct or interface (name), or end of the file (default; creates the file). "+
		"action=delete: name only; refuses while the symbol is used (force=true deletes anyway). imports=[...] adds the imports the new code needs; imports left unused by the edit are dropped. "+
		"Writes when the new errors stay within max_new_errors (default 0); apply=false previews; undo_edit reverts.",
		editSymbol)

	addWrite(s, h, readOnly, "rename_symbol", "Rename a symbol everywhere it is used (gopls rename), type-checked. Give name (Square.Area, shapes.Scale; file_path narrows) or file_path+line+column (1-indexed). "+
		"linked=true (default): renaming a method also renames the interface methods it satisfies and every implementation, since gopls leaves those behind and the program would stop compiling. "+
		"The result lists the linked methods and the places still using the old name (strings, comments, docs). Writes when new errors stay within max_new_errors (default 0); apply=false previews; undo_edit reverts.",
		func(ctx context.Context, w *Workspace, in renameIn) (string, error) {
			plan, err := planRename(ctx, w, in)
			if err != nil {
				return "", err
			}
			return finishPlan(ctx, w, plan, flag(in.Apply, true), limit(in.MaxNewErrors), flag(in.IncludeDependents, true), in.AllowLarge)
		})

	addWrite(s, h, readOnly, "quick_fix", "Apply gopls' quick fixes for a file's diagnostics (e.g. add a missing import, declare missing methods; an unused local variable gets `_ = x` after its declaration). Narrow with line or code. "+
		"Several different fixes for one diagnostic: nothing is applied, the options are listed; repeat with choice set to part of a title (a choice that fits only some diagnostics applies nothing unless strict=false). strict=true applies nothing unless every targeted diagnostic is fixed. The answer ends with `Applied:` and `Not fixed:` lines (with the reason per diagnostic). Missing imports are added together in one block. organize_imports=true instead adds missing and removes unused imports, sorted; fix_all=true applies gopls' source.fixAll to the whole file. Fixes gopls only offers as a command are reported, not applied. Type-checked like edit; undo_edit reverts.",
		quickFix)

	addWrite(s, h, readOnly, "change_signature", "Add, remove or reorder a function's or method's parameters and update every call site. "+
		"name: Scale, shapes.Scale or Square.Area. add=[{name,type,value,position?}] (value is the argument written at every existing call: Go has no defaults); remove=[names]; reorder=[all remaining names in the new order]; imports=[...] for the added types/values. "+
		"Call sites come from gopls references. include_overrides (default true) changes an interface method and its implementations together. Uses as a value, f(g()) with several results and xs... to a changing variadic are listed as manual work. "+
		"Writes when new errors stay within max_new_errors (default 0); apply=false previews; undo_edit reverts.",
		changeSignature)

	addWrite(s, h, readOnly, "move", "Move a top-level func, type, const or var to another file. Same directory: the declaration and the imports only it used move; "+
		"another directory (a package, created if missing): a type takes its methods along, package qualifiers are rewritten at every use (shapes.Square -> geom.Square), imports are fixed in every touched file. "+
		"Without name, file_path is a package directory and to_file its new directory: the .go files move (with the packages below) and every import of the old path is rewritten. "+
		"Refuses, with the reasons, when the moved code uses unexported identifiers that stay behind, when unexported members are used from outside, or when the move would create an import cycle. "+
		"Writes when new errors stay within max_new_errors (default 0); apply=false previews; undo_edit reverts.",
		moveSymbol)

	if !readOnly {
		addWrite(s, h, readOnly, "apply_edit", "Write a previewed edit by id, even if it adds errors. Refuses when a file changed since the preview.",
			func(ctx context.Context, w *Workspace, in applyIn) (string, error) {
				if eid, ok := w.Store.AppliedAs(in.ID); ok && w.Journal.WasUndone(eid) {
					return "", toolerr.Inputf("preview %q was applied as edit %s and has since been undone; run the refactoring again for a fresh preview.", in.ID, eid)
				}
				plan, err := w.Store.Get(in.ID)
				if err != nil {
					return "", err
				}
				if err := w.Journal.CheckFresh(plan); err != nil {
					return "", err
				}
				allowLarge := in.AllowLarge || plan.AllowLarge
				if err := w.Journal.Guard.CheckPlan(plan, allowLarge); err != nil {
					return "", err
				}
				delta, err := Simulate(ctx, w, plan, true)
				if err != nil {
					return "", err
				}
				plan.Holds = nil // the holds were shown with the preview; apply_edit writes it anyway
				out, editID, err := conclude(w, plan, delta, true, -1, allowLarge)
				if err != nil {
					return "", err
				}
				if editID != "" {
					w.Store.MarkApplied(in.ID, editID)
				}
				return out, nil
			})

		addWrite(s, h, readOnly, "undo_edit", "Revert an applied edit (the latest by default). Refuses when its files changed afterwards.",
			func(ctx context.Context, w *Workspace, in undoIn) (string, error) {
				e, err := w.Journal.Undo(in.ID, ReadOnlyEnv)
				if err != nil {
					return "", err
				}
				_ = w.Client.NoteWritten(planPaths(e.Plan))
				return fmt.Sprintf("Reverted edit %s: %s\n%s", e.ID, e.Plan.Description, e.Plan.Summary(w.Root)), nil
			})
	}

	addWrite(s, h, readOnly, "verify_changes", "Diagnostics delta of the uncommitted changes against git HEAD: new vs fixed errors in changed Go files and the packages importing them.",
		func(ctx context.Context, w *Workspace, in verifyIn) (string, error) {
			return verifyChanges(ctx, w, flag(in.IncludeDependents, true))
		})
}

var _ = format.Relative
