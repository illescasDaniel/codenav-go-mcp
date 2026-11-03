package server

import (
	"fmt"
	"strings"
	"testing"
)

// REN-007: the blank identifier as a new name gets the friendly message, not gopls' "invalid identifier" text.
func TestBlock20ERenameToBlankIdentifierIsRefused(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "ren20e/p.go", "package ren20e\n\nfunc Target20() int { return 1 }\n")
	out := call(t, cs, "rename_symbol", map[string]any{"name": "Target20", "new_name": "_", "apply": false})
	if !strings.Contains(out, `"_" is the blank identifier and cannot be a new name; pick a name.`) {
		t.Errorf("new_name=_ must get the blank-identifier message:\n%s", out)
	}
	if strings.Contains(out, "invalid identifier to rename") {
		t.Errorf("gopls text leaked:\n%s", out)
	}
}

// LANG-002: a named result is not a parameter; the refusal says so and gives the position form.
func TestBlock20ENamedResultIsNotAParameter(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "named20e/split.go", "package named20e\n\nfunc Split20(s string) (head, tail string) {\n\thead = s[:1]\n\ttail = s[1:]\n\treturn\n}\n")
	out := call(t, cs, "rename_symbol", map[string]any{"name": "Split20", "parameter": "head", "new_name": "first", "apply": false})
	for _, want := range []string{"`head` is a named result, not a parameter", "rename it by position", "file_path=named20e/split.go", "line=3"} {
		if !strings.Contains(out, want) {
			t.Errorf("named-result refusal lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "it has: s") {
		t.Errorf("the parameter list must not stand in for a named result:\n%s", out)
	}
}

// LANG-006: renaming Scan20 leaves ExampleScan20 and ExampleScan20_more naming nothing; the answer lists them.
// The method Box20.Area20 has ExampleBox20_Area20, listed the same way.
func TestBlock20EExampleNamesAreListed(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "lexample20e/parse.go", "package lexample20e\n\nfunc Scan20() int { return 1 }\n\ntype Box20 struct{}\n\nfunc (Box20) Area20() int { return 1 }\n")
	writeFile(t, root, "lexample20e/parse_test.go", "package lexample20e\n\nimport \"testing\"\n\nfunc ExampleScan20() {}\n\nfunc ExampleScan20_more() {}\n\nfunc ExampleBox20_Area20() {}\n\nfunc TestScan20(t *testing.T) {}\n")
	out := call(t, cs, "rename_symbol", map[string]any{"name": "Scan20", "new_name": "Scan21", "apply": false})
	for _, want := range []string{"ExampleScan20 to ExampleScan21 (parse_test.go:5)", "ExampleScan20_more to ExampleScan21_more"} {
		if !strings.Contains(out, want) {
			t.Errorf("the example names must be listed (%q):\n%s", want, out)
		}
	}
	if strings.Contains(out, "TestScan20 to") {
		t.Errorf("a Test function is not an example:\n%s", out)
	}
	out = call(t, cs, "rename_symbol", map[string]any{"name": "Box20.Area20", "new_name": "Area21", "apply": false})
	if !strings.Contains(out, "ExampleBox20_Area20 to ExampleBox20_Area21") {
		t.Errorf("the method's example must be listed:\n%s", out)
	}
	if got := readFile(t, root, "lexample20e/parse_test.go"); !strings.Contains(got, "func ExampleScan20() {}") {
		t.Errorf("the example is listed, not rewritten (apply=false wrote nothing):\n%s", got)
	}
}

// LANG-003: a parameter rename leaves the shadowing k of the closure alone; the note does not list it as unlinked.
func TestBlock20EShadowingLocalIsNotAnUnlinkedLeftover(t *testing.T) {
	cs, root := session(t, nil)
	const src = "package shadow20e\n\nfunc Count(k int) int {\n\tf := func() int {\n\t\tk := 2\n\t\treturn k\n\t}\n\treturn k + f()\n}\n"
	writeFile(t, root, "shadow20e/s.go", src)
	out := call(t, cs, "rename_symbol", map[string]any{"name": "Count", "parameter": "k", "new_name": "n", "apply": false})
	if !strings.Contains(out, "rename k -> n") {
		t.Fatalf("the parameter rename itself failed:\n%s", out)
	}
	if strings.Contains(out, "still appears as code") {
		t.Errorf("a shadowing local is another object, not an unlinked leftover:\n%s", out)
	}
}

// WRX-004: a symbol declared outside the workspace (GOROOT here, the module cache for a user) is named with the
// `../` form of the other refusals, never the absolute path.
func TestBlock20EOutsideDeclarationPathIsRelative(t *testing.T) {
	cs, root := session(t, nil)
	const src = "package wrx20e\n\nimport \"strings\"\n\nfunc Up20(s string) string { return strings.ToUpper(s) }\n"
	writeFile(t, root, "wrx20e/main.go", src)
	line := strings.Split(src, "\n")[4]
	col := strings.Index(line, "ToUpper") + 1
	out := call(t, cs, "rename_symbol", map[string]any{"file_path": "wrx20e/main.go", "line": 5, "column": col, "new_name": "Shout", "apply": false})
	if !strings.Contains(out, "is declared in ../") || !strings.Contains(out, "outside the workspace") {
		t.Fatalf("the outside declaration must be named with ../ steps:\n%s", out)
	}
	if strings.Contains(out, "declared in /") {
		t.Errorf("an absolute path leaked:\n%s", out)
	}
}

// SCALE-003: a leftover scan that stops at its file cap says so in the answer.
func TestBlock20ELeftoverScanCapIsNoted(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "scale20e/s.go", "package scale20e\n\nfunc Tally20() int { return 1 }\n")
	for i := 0; i < 2001; i++ {
		writeFile(t, root, fmt.Sprintf("bulk20e/f%04d.txt", i), "filler\n")
	}
	out := call(t, cs, "rename_symbol", map[string]any{"name": "Tally20", "new_name": "Tally21", "apply": false})
	if !strings.Contains(out, "leftover scan stopped after 2000 files; non-Go files past that point were not checked") {
		t.Errorf("the capped scan must be announced:\n%s", out)
	}
}

// SHAPE-006: with GOWORK=off a rename of an exported symbol that a sibling module uses says the sibling was not rewritten.
func TestBlock20EGoworkOffNamesTheSiblingModule(t *testing.T) {
	cs, root := session(t, map[string]string{"GOWORK": "off"})
	writeFile(t, root, "gw20e/g.go", "package gw20e\n\nfunc Ship20() int { return 1 }\n")
	writeFile(t, root, "sib20e/go.mod", "module example.com/fx/sib20e\n\ngo 1.24\n")
	writeFile(t, root, "sib20e/use.go", "package sib20e\n\nimport \"example.com/fx/gw20e\"\n\nvar _ = gw20e.Ship20\n")
	out := call(t, cs, "rename_symbol", map[string]any{"name": "gw20e.Ship20", "new_name": "Launch20", "apply": false})
	if !strings.Contains(out, "GOWORK=off: sibling modules are not visible, so their implementers and callers were not rewritten") {
		t.Errorf("the GOWORK=off note is missing:\n%s", out)
	}
	if !strings.Contains(out, "sib20e") {
		t.Errorf("the note must name the sibling module:\n%s", out)
	}
}
