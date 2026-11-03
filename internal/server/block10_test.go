package server

import (
	"strings"
	"testing"
)

// Regression tests for the sibling parse-error note and the diagnostics wording (DIAG-001, 002, 003, 008), run
// against gopls.

// BUG-DIAG-001: a clean file whose package has a syntax error in a sibling says so, the broken file itself does not
// get the note.
func TestSiblingSyntaxErrorIsNotedForCleanFile(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "d2/broken.go", "package d2\n\nfunc Broken( {\n\treturn\n}\n\nfunc Helper() int { return 1 }\n")
	writeFile(t, root, "d2/clean.go", "package d2\n\nfunc Use() int { return Helper() + 1 }\n")
	out := call(t, cs, "diagnostics", map[string]any{"file_path": "d2/clean.go"})
	mustHave(t, "clean file", out, "(note: package has a syntax error in d2/broken.go:3:14;")
	out = call(t, cs, "diagnostics", map[string]any{"file_path": "d2/broken.go"})
	if strings.Contains(out, "package has a syntax error") {
		t.Errorf("the broken file gets no sibling note:\n%s", out)
	}
	mustHave(t, "broken file", out, "expected ')'")
}

// BUG-DIAG-003: an analyzer finding names its analyzer (gopls gives the code "default", the source is printf).
func TestDiagnosticsNamesTheAnalyzer(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "an/an.go", "package an\n\nimport \"fmt\"\n\nfunc P() { fmt.Printf(\"%d\", \"s\") }\n")
	out := call(t, cs, "diagnostics", map[string]any{"file_path": "an/an.go"})
	mustHave(t, "printf finding", out, "[warning printf] fmt.Printf format %d has arg")
	if strings.Contains(out, "default") {
		t.Errorf("the analyzer's name is shown, not gopls' \"default\":\n%s", out)
	}
}

// BUG-DIAG-008: the other declaration of a redeclared name is shown with its workspace-relative location.
func TestDiagnosticsShowsOtherDeclaration(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "dup/a.go", "package dup\n\nfunc dup() {}\n")
	writeFile(t, root, "dup/b.go", "package dup\n\nfunc dup() {}\n")
	out := call(t, cs, "diagnostics", map[string]any{"file_path": "dup/b.go"})
	mustHave(t, "redeclared", out, "redeclared in this block", "dup/a.go:3:6: other declaration of dup")
	if strings.Contains(out, "file://") {
		t.Errorf("a file:// URI leaked:\n%s", out)
	}
}

// BUG-DIAG-002: a file whose import is in an import cycle says so in gopls' cycle terms, not "missing metadata".
func TestImportCycleWording(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "cyc/a/a.go", "package a\n\nimport _ \"example.com/fx/cyc/b\"\n")
	writeFile(t, root, "cyc/b/b.go", "package b\n\nimport _ \"example.com/fx/cyc/a\"\n")
	out := call(t, cs, "diagnostics", map[string]any{"file_path": "cyc/a/a.go"})
	mustHave(t, "cycle", out, "import cycle not allowed", "its package is part of an import cycle")
	if strings.Contains(out, "missing metadata") {
		t.Errorf("gopls' wording is rewritten:\n%s", out)
	}
}
