package server

import (
	"strings"
	"testing"
)

// LEAK-007 (batch Z): each refusal ends with a concrete next step. Checked through the session, so the text the
// caller sees is what is asserted.
func TestBlock20ZRefusalsEndWithNextStep(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "z20/z.go", "package z20\n\nfunc Z20Name() {}\n")
	writeFile(t, root, "z20/notes.txt", "notes\n")
	cases := []struct {
		name string
		tool string
		args map[string]any
		next string // the next-step phrase the refusal must contain
	}{
		{"non-go file", "hover", map[string]any{"file_path": "z20/notes.txt", "line": 1, "column": 1}, "use your normal file tools"},
		{"rename to the same name", "rename_symbol", map[string]any{"name": "Z20Name", "new_name": "Z20Name", "apply": false}, "pass a new_name that differs"},
		{"move into the file it is in", "move", map[string]any{"name": "Z20Name", "to_file": "z20/z.go", "apply": false}, "pass a different to_file"},
		{"move to a non-go file", "move", map[string]any{"name": "Z20Name", "to_file": "z20/notes.txt", "apply": false}, "use your normal file tools"},
		{"unknown preview id", "apply_edit", map[string]any{"id": "00000000"}, "run edit with apply=false first"},
		{"unknown edit id", "undo_edit", map[string]any{"id": "zz"}, "pass the id an applied edit reported"},
		{"line 0", "hover", map[string]any{"file_path": "z20/z.go", "line": 0, "column": 1}, "lines are 1-based"},
		{"column out of range", "hover", map[string]any{"file_path": "z20/z.go", "line": 3, "column": 999}, "valid columns are 1 to"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, isErr := callIsError(t, cs, tc.tool, tc.args)
			if !isErr {
				t.Fatalf("%s: expected a refusal, got:\n%s", tc.tool, out)
			}
			if !strings.Contains(out, tc.next) {
				t.Errorf("%s refusal lacks the next step %q:\n%s", tc.tool, tc.next, out)
			}
		})
	}
}
