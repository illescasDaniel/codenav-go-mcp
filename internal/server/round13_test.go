package server

import (
	"strings"
	"testing"
)

// Round 13 regression tests (one block per fix).

// Item 10: a file that mixes CRLF and LF keeps the ending of every untouched line, and inserted lines
// take the ending of their surroundings, for every write tool (including a CRLF/LF tie).
const (
	mixCRLF = "package mx\r\n\r\nfunc A() int {\r\n\treturn 1\r\n}\r\n\r\nfunc B() int {\n\treturn 1\n}\n" // CRLF majority
	mixTie  = "package mx\r\n\r\nfunc A() int {\r\n\treturn 1\r\n}\n\nfunc B() int {\n\treturn 1\n}\n"     // CRLF and LF tie
	mixLF   = "package mx\n\nfunc A() int {\n\treturn 1\n}\n\nfunc B() int {\r\n\treturn 1\r\n}\r\n"       // LF majority
	mixNoNL = "package mx\r\n\r\nfunc A() int {\r\n\treturn 1\r\n}\r\n\r\nfunc B() int {\n\treturn 1\n}"   // last line without ending
)

func TestMixedEOLEditKeepsUntouchedLines(t *testing.T) {
	for _, c := range []struct {
		name, src, old, new, want string
		all                       bool
	}{
		{"tie, insert in A", mixTie, "func A() int {\n\treturn 1", "func A() int {\n\ta := 1\n\treturn a", "package mx\r\n\r\nfunc A() int {\r\n\ta := 1\r\n\treturn a\r\n}\n\nfunc B() int {\n\treturn 1\n}\n", false},
		{"tie, insert in B", mixTie, "func B() int {\n\treturn 1", "func B() int {\n\ta := 1\n\treturn a", "package mx\r\n\r\nfunc A() int {\r\n\treturn 1\r\n}\n\nfunc B() int {\n\ta := 1\n\treturn a\n}\n", false},
		{"crlf majority, insert in A", mixCRLF, "func A() int {\n\treturn 1", "func A() int {\n\ta := 1\n\treturn a", "package mx\r\n\r\nfunc A() int {\r\n\ta := 1\r\n\treturn a\r\n}\r\n\r\nfunc B() int {\n\treturn 1\n}\n", false},
		{"lf majority, insert in B", mixLF, "func B() int {\n\treturn 1", "func B() int {\n\ta := 1\n\treturn a", "package mx\n\nfunc A() int {\n\treturn 1\n}\n\nfunc B() int {\r\n\ta := 1\r\n\treturn a\r\n}\r\n", false},
		{"insert at the start", mixCRLF, "package mx\n", "// Package mx.\npackage mx\n", "// Package mx.\r\npackage mx\r\n\r\nfunc A() int {\r\n\treturn 1\r\n}\r\n\r\nfunc B() int {\n\treturn 1\n}\n", false},
		{"insert at the end", mixTie, "func B() int {\n\treturn 1\n}\n", "func B() int {\n\treturn 1\n}\n\nfunc C() {}\n", "package mx\r\n\r\nfunc A() int {\r\n\treturn 1\r\n}\n\nfunc B() int {\n\treturn 1\n}\n\nfunc C() {}\n", false},
		{"last line without ending", mixNoNL, "func B() int {\n\treturn 1", "func B() int {\n\ta := 1\n\treturn a", "package mx\r\n\r\nfunc A() int {\r\n\treturn 1\r\n}\r\n\r\nfunc B() int {\n\ta := 1\n\treturn a\n}", false},
		{"replace_all", mixCRLF, "return 1", "a := 1\n\treturn a", "package mx\r\n\r\nfunc A() int {\r\n\ta := 1\r\n\treturn a\r\n}\r\n\r\nfunc B() int {\n\ta := 1\n\treturn a\n}\n", true},
	} {
		cs, root := session(t, nil)
		writeFile(t, root, "mx/mx.go", c.src)
		args := map[string]any{"file_path": "mx/mx.go", "old_string": c.old, "new_string": c.new}
		if c.all {
			args["replace_all"] = true
		}
		out := call(t, cs, "edit", args)
		if !strings.Contains(out, "Applied") {
			t.Errorf("%s: not applied:\n%s", c.name, out)
			continue
		}
		if got := readFile(t, root, "mx/mx.go"); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestMixedEOLSymbolRenameAndSignatureKeepUntouchedLines(t *testing.T) {
	// edit_symbol replace: the declaration's lines keep their endings, the added line follows them.
	cs, root := session(t, nil)
	writeFile(t, root, "mx/mx.go", mixTie)
	out := call(t, cs, "edit_symbol", map[string]any{"action": "replace", "name": "mx.B", "source": "func B() int {\n\ta := 1\n\treturn a\n}"})
	mustHave(t, "edit_symbol replace", out, "Applied")
	if got, want := readFile(t, root, "mx/mx.go"), "package mx\r\n\r\nfunc A() int {\r\n\treturn 1\r\n}\n\nfunc B() int {\n\ta := 1\n\treturn a\n}\n"; got != want {
		t.Errorf("edit_symbol replace:\n got %q\nwant %q", got, want)
	}
	// edit_symbol insert after A (a CRLF function): the new lines follow their neighbour.
	writeFile(t, root, "mx/mx.go", mixCRLF)
	out = call(t, cs, "edit_symbol", map[string]any{"action": "insert", "file_path": "mx/mx.go", "name": "mx.B", "position": "before", "source": "func Mid() int {\n\treturn 3\n}"})
	mustHave(t, "edit_symbol insert", out, "Applied")
	got := readFile(t, root, "mx/mx.go")
	if !strings.HasPrefix(got, "package mx\r\n\r\nfunc A() int {\r\n\treturn 1\r\n}\r\n") || !strings.HasSuffix(got, "func B() int {\n\treturn 1\n}\n") {
		t.Errorf("edit_symbol insert changed an untouched line: %q", got)
	}
	// rename and change_signature across a mixed file.
	writeFile(t, root, "mx/mx.go", "package mx\r\n\r\nfunc A(n int) int {\r\n\treturn n\r\n}\r\n\r\nfunc B() int {\n\treturn A(1)\n}\n")
	out = call(t, cs, "rename_symbol", map[string]any{"name": "mx.A", "new_name": "AA"})
	mustHave(t, "rename", out, "Applied")
	if got, want := readFile(t, root, "mx/mx.go"), "package mx\r\n\r\nfunc AA(n int) int {\r\n\treturn n\r\n}\r\n\r\nfunc B() int {\n\treturn AA(1)\n}\n"; got != want {
		t.Errorf("rename:\n got %q\nwant %q", got, want)
	}
	out = call(t, cs, "change_signature", map[string]any{"name": "mx.AA", "add": []any{map[string]any{"name": "m", "type": "int", "value": "2"}}})
	mustHave(t, "change_signature", out, "Applied")
	if got, want := readFile(t, root, "mx/mx.go"), "package mx\r\n\r\nfunc AA(n, m int) int {\r\n\treturn n\r\n}\r\n\r\nfunc B() int {\n\treturn AA(1, 2)\n}\n"; got != want {
		t.Errorf("change_signature:\n got %q\nwant %q", got, want)
	}
}

// Item 3: 2^63 does not fit an int; the answer names the number, never the JSON that follows it.
func TestOverflowIntegerMessageHasNoJSONTail(t *testing.T) {
	cs, _ := session(t, nil)
	big := uint64(1) << 63
	for name, c := range map[string]struct {
		tool string
		args map[string]any
		want string
	}{
		"column":         {"hover", map[string]any{"file_path": "shapes/shapes.go", "line": 3, "column": big}, "column must be a whole number of reasonable size"},
		"max_new_errors": {"rename_symbol", map[string]any{"name": "shapes.Scale", "new_name": "S2", "max_new_errors": big}, "max_new_errors must be a whole number of reasonable size"},
		"edit max":       {"edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "x", "new_string": "y", "max_new_errors": big}, "max_new_errors must be a whole number of reasonable size"},
	} {
		out := call(t, cs, c.tool, c.args)
		mustHave(t, name, out, c.want, "Usage: "+c.tool+"(")
		for _, bad := range []string{"file_path\":", "\"name\"", "...", "9223372036854776000", "overflows"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: %q leaked:\n%s", name, bad, out)
			}
		}
	}
}

// Item 1: cgo's synthetic callers (_cgoexp_<hash>_f, in the go build cache) are not callers anybody wrote.
// Item 2: hover on C.x explains the C symbol instead of printing a raw LSP failure with a file:// URI.
// Item 11 (_Ctype_int): symbol_info shows C.int.
func TestCgoExportCallersHoverAndTypes(t *testing.T) {
	skipWithoutCgo(t)
	cs, root := session(t, nil)
	writeFile(t, root, "exp/exp.go", "package exp\n\n/*\n#include <stdint.h>\nextern int goCallback(int);\nstatic int callit(int x) { return goCallback(x); }\n*/\nimport \"C\"\n\n//export goCallback\nfunc goCallback(x C.int) C.int { return x * 2 }\n\nfunc Run() int { return int(C.callit(3)) }\n")
	out := call(t, cs, "callers", map[string]any{"name": "exp.goCallback"})
	for _, bad := range []string{"_cgoexp_", "go-build", ".cache"} {
		if strings.Contains(out, bad) {
			t.Errorf("synthetic caller leaked (%q):\n%s", bad, out)
		}
	}
	mustHave(t, "callers", out, "No callers found")
	out = call(t, cs, "symbol_info", map[string]any{"name": "exp.goCallback"})
	for _, bad := range []string{"_cgoexp_", "go-build", "_Ctype_"} {
		if strings.Contains(out, bad) {
			t.Errorf("symbol_info leaked %q:\n%s", bad, out)
		}
	}
	mustHave(t, "symbol_info", out, "func goCallback(x C.int) C.int")
	out = call(t, cs, "hover", map[string]any{"file_path": "exp/exp.go", "line": 13, "column": 30})
	for _, bad := range []string{"file://", "LSP error", "no package data"} {
		if strings.Contains(out, bad) {
			t.Errorf("hover on C.callit shows raw %q:\n%s", bad, out)
		}
	}
	mustHave(t, "hover", out, "C.callit is C code")
}

// Items 5 and 6: a name lookup in a workspace that loads no package, and quick_fix on a file of a broken
// go.mod, answer in plain words: no raw LSP text, no file:// URI, no mention of the lookup's scratch directory.
func TestNoPackageLoadedFriendlyMessages(t *testing.T) {
	for _, c := range []struct{ name, files string }{
		{"empty", ""},
		{"nomod", "x.go"},
	} {
		root := t.TempDir()
		if c.files != "" {
			writeFile(t, root, c.files, "package x\n")
		}
		cs, _ := sessionAt(t, root)
		for _, q := range []struct{ tool, name string }{{"symbol_info", "fmt.Println"}, {"callers", "json.Marshal"}, {"implementations", "io.Reader"}} {
			out := call(t, cs, q.tool, map[string]any{"name": q.name})
			mustHave(t, c.name+" "+q.tool, out, "No Go package is loaded in this workspace")
			for _, bad := range []string{"file://", "LSP error", "codenav_probe", "no package metadata"} {
				if strings.Contains(out, bad) {
					t.Errorf("%s %s: %q leaked:\n%s", c.name, q.tool, bad, out)
				}
			}
		}
	}
	root := t.TempDir()
	writeFile(t, root, "go.mod", "this is not a go.mod\n")
	writeFile(t, root, "x.go", "package x\n")
	cs, _ := sessionAt(t, root)
	out := call(t, cs, "quick_fix", map[string]any{"file_path": "x.go", "line": 1, "code": "ZZZ"})
	mustHave(t, "quick_fix", out, "x.go is not part of any package gopls could load")
	for _, bad := range []string{"file://", "LSP error", "no package metadata", root} {
		if strings.Contains(out, bad) {
			t.Errorf("quick_fix: %q leaked:\n%s", bad, out)
		}
	}
}

// Item 4: a root two levels below the go.work directory shows the files of the other members as
// "../../x", for every tool (it used to print absolute paths).
func TestRootBelowGoWorkShowsRelativePaths(t *testing.T) {
	top := t.TempDir()
	writeFile(t, top, "go.work", "go 1.24\n\nuse (\n\t.\n\t./deps/dep\n)\n")
	writeFile(t, top, "go.mod", "module example.com/top\n\ngo 1.24\n")
	writeFile(t, top, "main.go", "package main\n\nimport \"example.com/dep\"\n\nfunc main() { _ = dep.New() }\n")
	writeFile(t, top, "deps/dep/go.mod", "module example.com/dep\n\ngo 1.24\n")
	writeFile(t, top, "deps/dep/dep.go", "package dep\n\ntype Client struct{}\n\ntype Doer interface{ Do() }\n\nfunc (Client) Do() {}\n\nfunc New() *Client { return &Client{} }\n")
	writeFile(t, top, "other/o.go", "package other\n\nimport \"example.com/dep\"\n\nfunc O() { _ = dep.New() }\n")
	writeFile(t, top, "other/go.mod", "module example.com/other\n\ngo 1.24\n\nrequire example.com/dep v0.0.0\n")
	root := top + "/deps/dep"
	cs, _ := sessionAt(t, root)
	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"callers", map[string]any{"name": "dep.New"}, "(../../main.go:5)"},
		{"symbol_info", map[string]any{"name": "dep.New"}, "../../main.go: L5"},
	} {
		out := call(t, cs, c.tool, c.args)
		mustHave(t, c.tool, out, c.want)
		if strings.Contains(out, top) {
			t.Errorf("%s: absolute path leaked:\n%s", c.tool, out)
		}
	}
}

// Item 8: callers is capped like references and search_symbol.
func TestCallersAreCapped(t *testing.T) {
	cs, root := session(t, nil)
	var sb strings.Builder
	sb.WriteString("package shapes\n\n")
	for i := 0; i < 70; i++ {
		sb.WriteString("func Caller" + strings.Repeat("A", 0) + string(rune('a'+i/26)) + string(rune('a'+i%26)) + "() float64 { return Scale(nil, 1, 0) }\n\n")
	}
	writeFile(t, root, "shapes/many.go", sb.String())
	out := call(t, cs, "callers", map[string]any{"name": "shapes.Scale"})
	mustHave(t, "callers", out, "… and 21 more caller(s)")
	if n := strings.Count(out, "[function]"); n != 50 {
		t.Errorf("want 50 callers listed, got %d:\n%s", n, out)
	}
}

// Item 9: renaming a package-level func does not list same-named struct fields, selectors, keyed
// literals or methods as "code the type checker did not link": they are other objects.
func TestRenameMentionsIgnoreFieldsSelectorsAndMethods(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "shad/shad.go", "package shad\n\nfunc foo(a int) int { return a }\n\ntype T struct{ foo int }\n\nfunc (t T) Method() int { return t.foo }\n\nfunc (t T) foo2() int { return foo(1) }\n\nfunc (T) foo() {}\n\nfunc Keyed() T {\n\treturn T{foo: 1}\n}\n\nvar tbl = map[string]int{\"k\": foo(2)}\n")
	out := call(t, cs, "rename_symbol", map[string]any{"name": "shad.foo", "new_name": "bar", "apply": false})
	mustHave(t, "rename", out, "func bar(a int) int", "return bar(1)", "bar(2)")
	if strings.Contains(out, "did not link") || strings.Contains(out, "still appears") {
		t.Errorf("fields, selectors, keyed literals and methods named foo are other objects than the func:\n%s", out)
	}
}

// Item 7: the "NOT rewritten" note appears only when a sibling module (no go.work) uses what the edit
// changes. Used and unused symbols of the same module, and a body-only edit of the used one.
func TestSiblingNoteOnlyForSymbolsTheSiblingUses(t *testing.T) {
	const note = "NOT rewritten"
	cs, root := session(t, nil)
	noWorkFixture(t, root)
	writeFile(t, root, "outdep/o.go", "package outdep\n\nfunc Hello() string { return \"hi\" }\n\nfunc Unref() int { return 1 }\n\nfunc Other() int { return 2 }\n")
	for name, c := range map[string]struct {
		tool string
		args map[string]any
		note bool
	}{
		"rename used":         {"rename_symbol", map[string]any{"name": "outdep.Hello", "new_name": "Hello2", "apply": false}, true},
		"rename unused":       {"rename_symbol", map[string]any{"name": "outdep.Unref", "new_name": "Unref2", "apply": false}, false},
		"signature used":      {"change_signature", map[string]any{"name": "outdep.Hello", "add": []any{map[string]any{"name": "n", "type": "int", "value": "1"}}, "apply": false}, true},
		"signature unused":    {"change_signature", map[string]any{"name": "outdep.Unref", "add": []any{map[string]any{"name": "n", "type": "int", "value": "1"}}, "apply": false}, false},
		"move unused":         {"move", map[string]any{"name": "outdep.Other", "to_file": "outdep/other.go", "apply": false}, false},
		"edit body of unused": {"edit", map[string]any{"file_path": "outdep/o.go", "old_string": "return 2", "new_string": "return 3", "apply": false}, false},
		"edit body of used":   {"edit", map[string]any{"file_path": "outdep/o.go", "old_string": `"hi"`, "new_string": `"hello"`, "apply": false}, false},
		"edit signature used": {"edit", map[string]any{"file_path": "outdep/o.go", "old_string": "func Hello() string", "new_string": "func Hello() (string, error)", "apply": false}, true},
	} {
		out := call(t, cs, c.tool, c.args)
		if has := strings.Contains(out, note); has != c.note {
			t.Errorf("%s: note shown = %v, want %v:\n%s", name, has, c.note, out)
		}
		if c.note {
			mustHave(t, name, out, "belongs to module example.com/outdep")
		}
	}
}
