package server

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// EDIT-001: include_dependents=false names the importers as not checked, next to the count; it is a disclosure,
// not a hold, so the preview is not a NOT CHECKED one.
func TestDependentsOffSaysImportersNotChecked(t *testing.T) {
	cs, _ := session(t, nil)
	out := call(t, cs, "edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "// Shape is a port.", "new_string": "// Shape is a port!", "apply": false, "include_dependents": false})
	mustHave(t, "dependents off", out, "importing packages NOT checked because include_dependents=false", "run verify_changes(include_dependents=true)")
	if strings.HasPrefix(out, "NOT CHECKED") || strings.Contains(out, "NOT CHECKED:") {
		t.Errorf("include_dependents=false is not a hold:\n%s", out)
	}
}

// WRX-002 / BED-001: a package that does not load (a module the workspace does not require) is never reported
// as checked with "0 new error(s)", with dependents on or off.
func TestEditInPackageWithMissingModuleIsNotChecked(t *testing.T) {
	const broken = "package shapes\n\nimport x \"example.com/notthere/x\"\n\nvar _ = x.Y\n"
	for _, deps := range []bool{true, false} {
		cs, root := session(t, nil)
		writeFile(t, root, "shapes/zz_wrx.go", broken)
		out := call(t, cs, "edit", map[string]any{"file_path": "shapes/zz_wrx.go", "old_string": "var _ = x.Y", "new_string": "var _ = x.Y\n\nvar _ int = x.Y + \"s\"", "apply": false, "include_dependents": deps})
		if !strings.Contains(out, "NOT CHECKED") || strings.Contains(out, "0 new error(s)") {
			t.Errorf("include_dependents=%v: want NOT CHECKED and no clean count, got:\n%s", deps, out)
		}
	}
}

// WRX-002: the rename variant of the same package is held too, not applied with "0 new error(s)".
func TestRenameInPackageWithMissingModuleIsHeld(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "shapes/zz_wrx.go", "package shapes\n\nimport x \"example.com/notthere/x\"\n\nvar _ = x.Y\n")
	before := readFile(t, root, "shapes/shapes.go")
	out, isErr := callIsError(t, cs, "rename_symbol", map[string]any{"name": "Scale", "new_name": "Scale2", "apply": true})
	if !isErr || !strings.Contains(out, "NOT CHECKED") || !strings.Contains(out, "Not written") {
		t.Fatalf("held rename: want a refused NOT CHECKED preview, isError=%v:\n%s", isErr, out)
	}
	if readFile(t, root, "shapes/shapes.go") != before {
		t.Fatal("held rename wrote the file")
	}
}

// apply_edit reply: a write past new errors restates them before Applied.
func TestApplyEditRestatesNewErrors(t *testing.T) {
	cs, _ := session(t, nil)
	out := call(t, cs, "edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "// Shape is a port.", "new_string": "var _ int = \"s\"\n\n// Shape is a port.", "apply": false})
	applied, isErr := callIsError(t, cs, "apply_edit", map[string]any{"id": previewID(t, out)})
	if isErr {
		t.Fatalf("apply_edit should write the edit:\n%s", applied)
	}
	i, j := strings.Index(applied, "Written with 1 new error(s)."), strings.Index(applied, "Applied (edit id")
	if i < 0 || j < 0 || i > j {
		t.Errorf("apply_edit reply must restate the new errors before Applied:\n%s", applied)
	}
}

// apply_edit reply: the signature holds it writes past are restated before Applied, not dropped silently.
func TestApplyEditRestatesSignatureHold(t *testing.T) {
	const plat = "package bsig\n\nfunc Plat(n int) int { return n }\n\nfunc Run() int { return Plat(1) }\n"
	const win = "package bsig\n\nfunc WinRun() int { return Plat(2) }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "bsig/plat.go", plat)
	writeFile(t, root, "bsig/x_windows.go", win)
	add := []any{map[string]any{"name": "k", "type": "int", "value": "0"}}
	out := call(t, cs, "change_signature", map[string]any{"name": "bsig.Plat", "add": add})
	applied, isErr := callIsError(t, cs, "apply_edit", map[string]any{"id": previewID(t, out)})
	if isErr {
		t.Fatalf("apply_edit should write the held edit:\n%s", applied)
	}
	i, j := strings.Index(applied, "Held, written anyway"), strings.Index(applied, "Applied (edit id")
	if i < 0 || j < 0 || i > j || !strings.Contains(applied, "1 use(s) of Plat in files the build leaves out are not rewritten") {
		t.Errorf("apply_edit reply must restate the held change before Applied:\n%s", applied)
	}
}

// VERIFY-004: a change to more files than the check limit names the edited files left out and does not present
// the count as exact.
func TestVerifyChangesCapNamesEditedFiles(t *testing.T) {
	cs, root := session(t, nil)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "base"}} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	for i := 0; i < 65; i++ {
		writeFile(t, root, fmt.Sprintf("shapes/vg%02d.go", i), fmt.Sprintf("package shapes\n\nfunc vg%02d() {}\n", i))
	}
	out := call(t, cs, "verify_changes", map[string]any{})
	mustHave(t, "verify cap", out, "5 edited file(s)", "beyond the 60-file limit were not type-checked", "Diagnostics: 0 new error(s) in the checked files")
}

// apply_edit reply: a clean write (nothing restated) keeps the blank line before Applied, as before block 6.
func TestApplyEditCleanReplyKeepsBlankLine(t *testing.T) {
	cs, _ := session(t, nil)
	out := call(t, cs, "edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "// Shape is a port.", "new_string": "// Shape is a port!", "apply": false})
	applied, isErr := callIsError(t, cs, "apply_edit", map[string]any{"id": previewID(t, out)})
	if isErr || !strings.Contains(applied, "\n\nApplied (edit id") || strings.Contains(applied, "Written with") {
		t.Errorf("clean apply_edit reply: want a blank line before Applied and no restatement, isError=%v:\n%s", isErr, applied)
	}
}
