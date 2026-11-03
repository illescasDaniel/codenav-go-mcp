package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/diagdelta"
	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/txn"
)

// block20BWorkspace is a workspace in a temporary directory holding files (name -> text); no language server.
func block20BWorkspace(t *testing.T, files map[string]string) *Workspace {
	t.Helper()
	root := t.TempDir()
	for name, text := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Workspace{Root: root, Journal: txn.NewJournal(txn.WriteGuard{Root: root}), Store: txn.NewPlanStore()}
}

func block20BEditErr(t *testing.T, w *Workspace, file, old, repl string) error {
	t.Helper()
	_, err := planEdit(w, editIn{FilePath: file, OldString: old, NewString: &repl})
	if err == nil {
		t.Fatalf("edit of %q: want a refusal, got a plan", old)
	}
	return err
}

// EDIT-002: the "matches N times" refusal names the line of every match.
func TestBlock20BMatchesNameTheirLines(t *testing.T) {
	const dup = "package calc\n\nfunc Dup() int {\n\ta := 0\n\treturn a\n}\n\nfunc Dup2() int {\n\ta := 0\n\treturn a\n}\n\nfunc Dup3() int {\n\ta := 0\n\treturn a\n}\n"
	w := block20BWorkspace(t, map[string]string{"calc/dup.go": dup})
	err := block20BEditErr(t, w, "calc/dup.go", "\ta := 0\n\treturn a", "\ta := 1\n\treturn a")
	if !strings.Contains(err.Error(), "matches 3 times") || !strings.Contains(err.Error(), "lines 4, 9, 14") {
		t.Errorf("want the three match lines (4, 9, 14), got %q", err)
	}
}

// EDIT-003: a space-indented old_string against a tab-indented file gets the tab hint and the closest line.
func TestBlock20BNotFoundTabHint(t *testing.T) {
	w := block20BWorkspace(t, map[string]string{"calc/use.go": "package calc\n\nfunc Use() int {\n\treturn Add(1, 2)\n}\n"})
	err := block20BEditErr(t, w, "calc/use.go", "    return Add(1, 2)", "    return Add(1, 3)")
	msg := err.Error()
	if !strings.Contains(msg, "Go files are indented with tabs") {
		t.Errorf("want the tab hint, got %q", msg)
	}
	if !strings.Contains(msg, `The closest line is 4: "\treturn Add(1, 2)"`) {
		t.Errorf("want the closest line 4 quoted with its tab, got %q", msg)
	}
	// Not space-indented: no tab hint, but the closest line still shows when only whitespace differs.
	err = block20BEditErr(t, w, "calc/use.go", "\treturn  Add(1, 2)", "\treturn Add(1, 3)")
	if strings.Contains(err.Error(), "indented with tabs") || !strings.Contains(err.Error(), "The closest line is 4") {
		t.Errorf("double space: want the closest line and no tab hint, got %q", err)
	}
	// Nothing close: the plain text only.
	err = block20BEditErr(t, w, "calc/use.go", "\treturn Sub(9)", "\treturn Sub(8)")
	if strings.Contains(err.Error(), "closest line") || strings.Contains(err.Error(), "indented with tabs") {
		t.Errorf("no close line: want the plain refusal, got %q", err)
	}
}

// EDIT-008: a non-Go file is refused with what to use instead, and go.mod / go.work say they are module files.
func TestBlock20BNotGoFileRefusalSaysWhatToDo(t *testing.T) {
	w := block20BWorkspace(t, map[string]string{"go.mod": "module example.com/x\n", "README.md": "# x\n"})
	for _, name := range []string{"go.mod", "README.md"} {
		err := block20BEditErr(t, w, name, "x", "y")
		if !strings.Contains(err.Error(), "normal file tools") {
			t.Errorf("%s: want the next step, got %q", name, err)
		}
	}
	if err := block20BEditErr(t, w, "go.mod", "x", "y"); !strings.Contains(err.Error(), "module file") {
		t.Errorf("go.mod: want the module-file wording, got %q", err)
	}
	if err := GoGuardRule(edits.FileChange{Path: filepath.Join(w.Root, "go.work")}); err == nil || !strings.Contains(err.Error(), "module file") || !strings.Contains(err.Error(), "normal file tools") {
		t.Errorf("go.work through the write guard: got %v", err)
	}
}

// ESYM-003: a new InvalidIfaceAssign error points at quick_fix, once, whatever the number of implementers.
// ESYM-004: an unused import after a delete points at prune_imports; the same error after another edit does not.
func TestBlock20BRefusalHints(t *testing.T) {
	iface := diagdelta.Delta{New: []diagdelta.Entry{
		{Path: "p014/p.go", Line: 20, Column: 16, Severity: diagdelta.ErrorSeverity, Code: "InvalidIfaceAssign", Message: "A does not implement Source (missing method Close)"},
		{Path: "p014/p.go", Line: 21, Column: 16, Severity: diagdelta.ErrorSeverity, Code: "InvalidIfaceAssign", Message: "B does not implement Source (missing method Close)"},
	}}
	if got := refusalHints("insert into p014/p.go", iface); len(got) != 1 || !strings.Contains(got[0], "quick_fix") {
		t.Errorf("InvalidIfaceAssign: want one quick_fix hint, got %q", got)
	}
	unused := diagdelta.Delta{New: []diagdelta.Entry{{Path: "p018/p.go", Line: 3, Column: 8, Severity: diagdelta.ErrorSeverity, Code: "UnusedImport", Message: `"strings" imported and not used`}}}
	if got := refusalHints("delete X", unused); len(got) != 1 || !strings.Contains(got[0], "prune_imports=true") {
		t.Errorf("delete with UnusedImport: want the prune_imports hint, got %q", got)
	}
	if got := refusalHints("replace X", unused); len(got) != 0 {
		t.Errorf("replace with UnusedImport: want no prune hint, got %q", got)
	}
	// A warning is not a new error: no hint.
	warn := diagdelta.Delta{New: []diagdelta.Entry{{Severity: 2, Code: "InvalidIfaceAssign"}}}
	if got := refusalHints("insert into p.go", warn); len(got) != 0 {
		t.Errorf("warning: want no hint, got %q", got)
	}
}

// EDIT-004: edit names the import change it does not make, for an undefined package qualifier or an unused import.
func TestBlock20BEditImportHint(t *testing.T) {
	added := "package strs\n\nfunc Name() string {\n\treturn strings.ToUpper(\"x\")\n}\n"
	plan := edits.NewPlan("edit strs/strs.go", []edits.FileChange{{Path: "/r/strs/strs.go", New: &added}})
	undeclared := diagdelta.Delta{New: []diagdelta.Entry{{Severity: diagdelta.ErrorSeverity, Code: "UndeclaredName", Message: "undefined: strings"}}}
	got := editImportHints(plan, undeclared)
	if len(got) != 1 || !strings.Contains(got[0], "does not manage imports") || !strings.Contains(got[0], "organize_imports=true") {
		t.Errorf("undefined package qualifier: got %q", got)
	}
	unused := diagdelta.Delta{New: []diagdelta.Entry{{Severity: diagdelta.ErrorSeverity, Code: "UnusedImport", Message: `"strings" imported and not used`}}}
	if got := editImportHints(plan, unused); len(got) != 1 {
		t.Errorf("unused import: want the hint, got %q", got)
	}
	// An undefined local that is not used as a qualifier is not an import problem.
	typo := diagdelta.Delta{New: []diagdelta.Entry{{Severity: diagdelta.ErrorSeverity, Code: "UndeclaredName", Message: "undefined: nmae"}}}
	if got := editImportHints(plan, typo); len(got) != 0 {
		t.Errorf("undefined local: want no import hint, got %q", got)
	}
}

// The hints reach the answer: a refused preview prints them after the diagnostics, as Hint lines.
func TestBlock20BHintsAreRendered(t *testing.T) {
	w := block20BWorkspace(t, nil)
	plan := edits.NewPlan("insert into p014/p.go", nil)
	delta := diagdelta.Delta{New: []diagdelta.Entry{{Path: "p014/p.go", Line: 20, Column: 16, Severity: diagdelta.ErrorSeverity, Code: "InvalidIfaceAssign", Message: "A does not implement Source"}}}
	out, _, err := conclude(w, plan, delta, false, 0, false, nil, refusalHints(plan.Description, delta))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Hint: quick_fix can declare the missing method") {
		t.Errorf("want the hint in the answer, got:\n%s", out)
	}
}

// EDIT-010 through the refusal itself: the refused answer carries the cut line, not the 200 KB line whole.
func TestBlock20BRefusedDiffCutsLongLine(t *testing.T) {
	w := block20BWorkspace(t, nil)
	long := "var s = \"" + strings.Repeat("a", 200000) + "\""
	old := "package long\n\n" + long + "\n"
	nw := "package long\n\nvar t = undefinedQ\n" + long + "\n"
	plan := edits.NewPlan("edit long/long.go", []edits.FileChange{{Path: filepath.Join(w.Root, "long/long.go"), Old: &old, New: &nw}})
	delta := diagdelta.Delta{New: []diagdelta.Entry{{Path: "long/long.go", Line: 3, Column: 5, Severity: diagdelta.ErrorSeverity, Code: "UndeclaredName", Message: "undefined: undefinedQ"}}}
	_, _, err := conclude(w, plan, delta, true, 0, false, nil, nil)
	if err == nil {
		t.Fatal("want the write refused: one new error and max_new_errors=0")
	}
	if msg := err.Error(); len(msg) > 2000 || !strings.Contains(msg, "more characters)") {
		t.Errorf("refused answer should cut the long line: %d bytes, got %.300s", len(msg), msg)
	}
}
