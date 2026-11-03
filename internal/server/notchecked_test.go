package server

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// BED-001 / ENV-002 / CONC-001 gate: an edit whose package does not load cannot be checked, so the write is held
// as a preview, the reply says NOT CHECKED (never a clean count), and apply_edit writes it restating the reason.
func TestNotCheckedHoldsEditUntilApplyEdit(t *testing.T) {
	cs, root := session(t, nil)
	// A file of package shapes importing a package that does not exist: shapes does not load.
	writeFile(t, root, "shapes/zz_missing.go", "package shapes\n\nimport _ \"example.com/fx/nothere\"\n")
	before := readFile(t, root, "shapes/shapes.go")

	out, isErr := callIsError(t, cs, "edit", map[string]any{
		"file_path": "shapes/shapes.go", "old_string": "func Scale(", "new_string": "// Scale scales.\nfunc Scale(",
	})
	if !isErr || !strings.Contains(out, "NOT CHECKED") || !strings.Contains(out, "Not written") {
		t.Fatalf("held edit: want a refused NOT CHECKED preview, isError=%v:\n%s", isErr, out)
	}
	if strings.Contains(out, "0 new error(s)") {
		t.Errorf("held edit must not claim a clean count:\n%s", out)
	}
	if readFile(t, root, "shapes/shapes.go") != before {
		t.Fatalf("held edit wrote the file")
	}

	m := regexp.MustCompile(`apply_edit\(id="([^"]+)"`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("held edit names no preview id:\n%s", out)
	}
	applied, isErr := callIsError(t, cs, "apply_edit", map[string]any{"id": m[1]})
	if isErr {
		t.Fatalf("apply_edit should write past the NOT CHECKED hold:\n%s", applied)
	}
	check := strings.Index(applied, "NOT CHECKED: ")
	written := strings.Index(applied, "Applied (edit id")
	if check < 0 || written < 0 || check > written {
		t.Errorf("apply_edit reply must restate the not-checked reason before Applied:\n%s", applied)
	}
	if got := readFile(t, root, "shapes/shapes.go"); !strings.Contains(got, "// Scale scales.") {
		t.Errorf("apply_edit did not write the edit")
	}
}

// VERIFY-002: a change to only a module file is said to be module-only, not "No uncommitted Go changes".
func TestVerifyChangesNamesModuleFileChange(t *testing.T) {
	cs, root := session(t, nil)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "base"}} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	writeFile(t, root, "go.mod", readFile(t, root, "go.mod")+"\n// touched\n")
	out := call(t, cs, "verify_changes", map[string]any{})
	if strings.Contains(out, "No uncommitted Go changes") || !strings.Contains(out, "Uncommitted changes vs HEAD in module file(s) only: go.mod") {
		t.Errorf("module-only change: want the module-only report, got:\n%s", out)
	}
}
