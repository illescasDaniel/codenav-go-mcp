package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Strengthening of the round-12 coverage (N6, N8, N9b, N9c, package-clause refusal).

// N6: one answer is capped at ~400 KB; the cut keeps whole lines when it can and says how much was
// left out. The edit preview of a file with one 500 KB line is such an answer.
func TestOversizedAnswerIsCappedWithOmittedCount(t *testing.T) {
	cs, root := session(t, nil)
	big := "package shapes\n\nvar Big = \"" + strings.Repeat("x", 500000) + "\"\n"
	out := call(t, cs, "edit", map[string]any{"file_path": "shapes/big.go", "new_text": big, "apply": false})
	if len(out) > 401_000 {
		t.Errorf("answer not capped: %d bytes", len(out))
	}
	mustHave(t, "cap", out, "more byte(s) omitted", "the answer is over 400000 bytes", "narrow the request")
	if !strings.Contains(out, "edit shapes/big.go") {
		t.Errorf("the head of the answer must be kept:\n%.300s", out)
	}
	// A normal answer is untouched.
	small := call(t, cs, "edit", map[string]any{"file_path": "shapes/small.go", "new_text": "package shapes\n\nvar Small = 1\n", "apply": false})
	if strings.Contains(small, "omitted") {
		t.Errorf("small answers are not capped:\n%s", small)
	}
	if _, err := os.Stat(filepath.Join(root, "shapes/big.go")); err == nil {
		t.Error("a preview writes nothing")
	}
}

// N8: gopls / type-checker messages show workspace-relative paths. "method Square.Area already declared
// at <abs>/shapes/c.go:3:18" is a message whose absolute path only the central rewrite (Workspace.scrub)
// removes: without it the path of the temp workspace shows.
func TestTypeCheckerMessageEmbeddedPathIsRelative(t *testing.T) {
	cs, root := session(t, nil)
	out := call(t, cs, "edit", map[string]any{"file_path": "shapes/c.go", "new_text": "package shapes\n\nfunc (s *Square) Area() float64 { return 1 }\n", "apply": false})
	mustHave(t, "dup method", out, "already declared at shapes/c.go:3:18")
	if strings.Contains(out, root) {
		t.Errorf("absolute workspace path leaked:\n%s", out)
	}
}

// N9b: the "NOT rewritten" note is shown only when a sibling module (no go.work) really uses the
// edited module: a used symbol: yes; a module nobody imports: no.
func TestSiblingModuleNoteOnlyWhenUsed(t *testing.T) {
	const note = "NOT rewritten"
	// Used: appmod calls outdep.Hello.
	cs, root := session(t, nil)
	noWorkFixture(t, root)
	writeFile(t, root, "lonely/go.mod", "module example.com/lonely\n\ngo 1.24\n")
	writeFile(t, root, "lonely/l.go", "package lonely\n\nfunc Alone() int { return 1 }\n")
	out := call(t, cs, "edit", map[string]any{"file_path": "outdep/o.go", "old_string": `"hi"`, "new_string": `"hello"`, "apply": false})
	mustHave(t, "used module", out, "belongs to module example.com/outdep", note)
	// Unused: no other module imports lonely, so nothing is left unedited.
	out = call(t, cs, "edit", map[string]any{"file_path": "lonely/l.go", "old_string": "return 1", "new_string": "return 2", "apply": false})
	if strings.Contains(out, note) || strings.Contains(out, "belongs to module") {
		t.Errorf("no sibling uses lonely, no note:\n%s", out)
	}
	// A file of the main module never gets it.
	out = call(t, cs, "edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "package shapes", "new_string": "package shapes // x", "apply": false})
	if strings.Contains(out, note) {
		t.Errorf("main module edit must not carry the note:\n%s", out)
	}
}

// N9c: the "module(s) ... sit below the workspace without a go.work" note belongs to lookups that found
// nothing and that the nested module could explain; not to successes or to other errors.
func TestNestedModuleNoteOnlyOnRelevantNotFound(t *testing.T) {
	const note = "sit below the workspace without a go.work"
	cs, root := session(t, nil)
	writeFile(t, root, "inner/go.mod", "module example.com/inner\n\ngo 1.24\n")
	writeFile(t, root, "inner/i.go", "package inner\n\nfunc InnerThing() int { return 1 }\n")
	// Relevant: the qualifier names the nested module's package, or the missing name appears in its code.
	for _, name := range []string{"inner.Missing", "InnerThingg", "inner.InnerThing2"} {
		out := call(t, cs, "rename_symbol", map[string]any{"name": name, "new_name": "X", "apply": false})
		if name == "InnerThingg" {
			// The word-boundary match needs the exact word: this is NOT relevant.
			if strings.Contains(out, note) {
				t.Errorf("%s: partial word is no reason to mention the module:\n%s", name, out)
			}
			continue
		}
		mustHave(t, name, out, note, "example.com/inner (inner)")
	}
	// The last part appears in the module's code: relevant although the qualifier names nothing there.
	out := call(t, cs, "rename_symbol", map[string]any{"name": "zzq.InnerThing", "new_name": "X", "apply": false})
	mustHave(t, "name in module code", out, note)
	// Irrelevant: success, another error, a name no file of the module contains.
	for name, c := range map[string]map[string]any{
		"success":        {"name": "shapes.Scale", "new_name": "Scale2", "apply": false},
		"bad identifier": {"name": "shapes.Scale", "new_name": "not valid", "apply": false},
		"unrelated miss": {"name": "QQQNothing", "new_name": "X", "apply": false},
	} {
		if out := call(t, cs, "rename_symbol", c); strings.Contains(out, note) {
			t.Errorf("%s: the note has nothing to do with this result:\n%s", name, out)
		}
	}
	if out := call(t, cs, "symbol_info", map[string]any{"name": "shapes.Scale"}); strings.Contains(out, note) {
		t.Errorf("a successful lookup carries no note:\n%s", out)
	}
}

// Package clause refusal, as implemented: refused (one message) while max_new_errors is 0 or 1; skipped
// (the caller accepts the transitional errors) for -1 and >= 2. A directory that already mixes packages,
// a go:build ignore file and files that are part of the same edit do not count as "the other files".
func TestPackageClauseRefusalBoundsAndDirectories(t *testing.T) {
	cs, root := session(t, nil)
	edit := func(maxNew any, file, text string) string {
		args := map[string]any{"file_path": file, "new_text": text, "apply": false}
		if maxNew != nil {
			args["max_new_errors"] = maxNew
		}
		return call(t, cs, "edit", args)
	}
	const refused = "would declare `package wrong`"
	// Several files of the directory all declare `package shapes` (consts.go, shapes.go, one more).
	writeFile(t, root, "shapes/third.go", "package shapes\n\nfunc Third() {}\n")
	for _, c := range []struct {
		maxNew  any
		refused bool
	}{{nil, true}, {0, true}, {1, true}, {2, false}, {5, false}, {-1, false}} {
		out := edit(c.maxNew, "shapes/new1.go", "package wrong\n\nfunc N1() {}\n")
		if got := strings.Contains(out, refused); got != c.refused {
			t.Errorf("max_new_errors=%v: refused=%v, want %v:\n%s", c.maxNew, got, c.refused, out)
		}
	}
	out := edit(nil, "shapes/new1.go", "package wrong\n\nfunc N1() {}\n")
	mustHave(t, "multi-file dir", out, "the other files of shapes declare `package shapes`", "consts.go", "pass max_new_errors=-1")
	// A directory that already mixes two packages: the check stays out of it.
	writeFile(t, root, "mixed/a.go", "package one\n")
	writeFile(t, root, "mixed/b.go", "package two\n")
	if out := edit(nil, "mixed/c.go", "package wrong\n"); strings.Contains(out, "would declare") {
		t.Errorf("a mixed directory is not judged:\n%s", out)
	}
	// A go:build ignore file does not define the package of the directory.
	writeFile(t, root, "gen2/real.go", "package real\n")
	writeFile(t, root, "gen2/tool.go", "//go:build ignore\n\npackage main\n")
	if out := edit(nil, "gen2/x.go", "package real\n\nfunc X() {}\n"); strings.Contains(out, "would declare") {
		t.Errorf("the ignored main file is not a second package:\n%s", out)
	}
	// An unchanged package clause is never judged, whatever the other files say.
	if out := edit(nil, "shapes/shapes.go", readFile(t, root, "shapes/shapes.go")+"\nfunc Same() {}\n"); strings.Contains(out, "would declare") {
		t.Errorf("same package clause:\n%s", out)
	}
}
