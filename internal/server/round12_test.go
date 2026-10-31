package server

import (
	"path/filepath"
	"strings"
	"testing"
)

// Regression tests for the fixes of round 12 (one test per fix).

// N1: a local variable or parameter that shadows the renamed package-level name is another object: the
// "still appears as code the type checker did not link" note must not list it.
func TestRenameMentionsIgnoreShadowingLocals(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "sh/sh.go", "package sh\n\nfunc foo() int { return 1 }\n\nfunc Use() int { return foo() }\n\nfunc Local() int {\n\tfoo := 5\n\treturn foo + 1\n}\n")
	out := call(t, cs, "rename_symbol", map[string]any{"name": "sh.foo", "new_name": "foo2", "apply": false})
	mustHave(t, "rename", out, "foo -> foo2", "func foo2() int")
	if strings.Contains(out, "did not link") || strings.Contains(out, "sh/sh.go:8") {
		t.Errorf("the local variable foo is not a leftover of the rename:\n%s", out)
	}
}

// N2: file_path + to_file without name moves a directory; for a file the answer says how to move a symbol.
func TestMoveFileWithoutNameExplains(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "big/f1.go", "package big\n\nfunc F1() {}\n")
	out := call(t, cs, "move", map[string]any{"file_path": "big/f1.go", "to_file": "mv4/f1.go", "apply": false})
	mustHave(t, "move file", out, "big/f1.go is a file", "moves a whole directory", "name=<symbol>")
	if strings.Contains(out, "is not a directory") {
		t.Errorf("the old message gives no way forward:\n%s", out)
	}
}

// N3 + N4: a miscased qualifier or member names the spelling that works, and says which part is wrong.
func TestMiscasedQualifierAndMemberSuggestions(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "csq/cs.go", "package csq\n\nfunc foo() int { return 1 }\n\nfunc Foo() int { return 2 }\n")
	writeFile(t, root, "tool/main.go", "package main\n\nfunc init1() int { return 1 }\n\nfunc main() { _ = init1() }\n")
	for _, name := range []string{"Csq.Foo", "CSQ.Foo"} {
		out := call(t, cs, "rename_symbol", map[string]any{"name": name, "new_name": "X", "apply": false})
		mustHave(t, name, out, "Did you mean `csq.Foo`?")
		if strings.Contains(out, "`csq.foo`") || strings.Contains(out, "Applied") {
			t.Errorf("%s: only the nearest spelling is suggested:\n%s", name, out)
		}
	}
	out := call(t, cs, "rename_symbol", map[string]any{"name": "main.Init1", "new_name": "X", "apply": false})
	mustHave(t, "main.Init1", out, "package main declares no Init1", "Did you mean `main.init1`?")
	if strings.Contains(out, "no type or package named main") {
		t.Errorf("package main exists:\n%s", out)
	}
}

// Round-11 partial: a package clause that differs from the other files of the directory is refused with
// ONE message (not the type checker's two error lines for one cause); the external test package and a
// new directory stay allowed, and max_new_errors=-1 allows renaming the package file by file.
func TestEditPackageClauseMismatchRefusedOnce(t *testing.T) {
	cs, root := session(t, nil)
	before := readFile(t, root, "shapes/shapes.go")
	out := call(t, cs, "edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "package shapes", "new_string": "package wrong", "apply": false})
	mustHave(t, "mismatch", out, "shapes/shapes.go would declare `package wrong`", "declare `package shapes`", "one directory is one package")
	if strings.Contains(out, "MismatchedPkgName") || strings.Contains(out, "different packages") || strings.Contains(out, "Diagnostics:") {
		t.Errorf("one friendly message, not the two type-checker lines:\n%s", out)
	}
	if readFile(t, root, "shapes/shapes.go") != before {
		t.Error("nothing may be written")
	}
	// A new file with a wrong package is refused too.
	out = call(t, cs, "edit", map[string]any{"file_path": "shapes/extra.go", "new_text": "package other\n\nfunc X() {}\n", "apply": false})
	mustHave(t, "new file", out, "would declare `package other`")
	// Allowed: the external test package, a new directory, and the opt-in file-by-file rename.
	out = call(t, cs, "edit", map[string]any{"file_path": "shapes/ext_test.go", "new_text": "package shapes_test\n\nfunc X() {}\n", "apply": false})
	if strings.Contains(out, "would declare") {
		t.Errorf("an external test package is fine:\n%s", out)
	}
	out = call(t, cs, "edit", map[string]any{"file_path": "newpkg/n.go", "new_text": "package whatever\n\nfunc X() {}\n", "apply": false})
	if strings.Contains(out, "would declare") {
		t.Errorf("a new directory takes any package name:\n%s", out)
	}
	out = call(t, cs, "edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "package shapes", "new_string": "package wrong", "apply": false, "max_new_errors": -1})
	if strings.Contains(out, "would declare") {
		t.Errorf("max_new_errors=-1 accepts the transitional errors:\n%s", out)
	}
}

// Cosmetic: an organize_imports preview does not repeat "Applied: nothing ... / Not fixed: none"; when it
// removes an import with its comment, no blank line appears above the next heading.
func TestOrganizeImportsPreviewTailAndBlankLine(t *testing.T) {
	cs, root := session(t, nil)
	src := "package shapes\n\nimport (\n\t\"fmt\"\n\t/* multi\n\t   line comment */\n\t\"errors\"\n\t// end\n\t\"os\"\n)\n\nfunc OI() { fmt.Println(os.Args) }\n"
	writeFile(t, root, "shapes/oi.go", src)
	out := call(t, cs, "quick_fix", map[string]any{"file_path": "shapes/oi.go", "organize_imports": true, "apply": false})
	mustHave(t, "preview", out, "Preview only", "-\t\"errors\"")
	if strings.Contains(out, "Applied: nothing") || strings.Contains(out, "Not fixed: none") {
		t.Errorf("a preview must not end with the redundant tail:\n%s", out)
	}
	if strings.Contains(out, "+\n") || strings.Contains(out, "\n+\t\n") {
		t.Errorf("no blank line may be added:\n%s", out)
	}
	out = call(t, cs, "quick_fix", map[string]any{"file_path": "shapes/oi.go", "organize_imports": true})
	mustHave(t, "applied", out, "Applied: Organize Imports", "Not fixed: none")
	want := "package shapes\n\nimport (\n\t\"fmt\"\n\t// end\n\t\"os\"\n)\n\nfunc OI() { fmt.Println(os.Args) }\n"
	if got := readFile(t, root, "shapes/oi.go"); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// N5: `_` (the blank identifier) is "nothing to rename" for every write tool, whatever is in the workspace.
func TestBlankIdentifierIsNothingToChange(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "shapes/blank.go", "package shapes\n\nvar _ = 1\n")
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"rename_symbol", map[string]any{"name": "_", "new_name": "X", "apply": false}},
		{"edit_symbol", map[string]any{"action": "delete", "name": "_", "apply": false}},
		{"change_signature", map[string]any{"name": "_", "remove": []any{"a"}, "apply": false}},
		{"move", map[string]any{"name": "_", "to_file": "shapes/z.go", "apply": false}},
	} {
		out := call(t, cs, c.tool, c.args)
		mustHave(t, c.tool, out, "`_` is the blank identifier", "nothing to rename")
		if strings.Contains(out, "symbols match") {
			t.Errorf("%s lists candidates for `_`:\n%s", c.tool, out)
		}
	}
}

// N5: results never list the glue cgo writes into the go build cache (symbol_info/search_symbol on `_`
// used to show ~/.cache/go-build/... locations).
func TestCgoGlueNotListed(t *testing.T) {
	skipWithoutCgo(t)
	cs, root := session(t, nil)
	writeFile(t, root, "cgb/cg.go", "package cgb\n\n/*\nstatic int add(int a, int b) { return a + b; }\n*/\nimport \"C\"\n\nvar _ = 1\n\nfunc Add(a, b int) int { return int(C.add(C.int(a), C.int(b))) }\n")
	for tool, args := range map[string]map[string]any{"symbol_info": {"name": "_"}, "search_symbol": {"query": "_"}} {
		out := call(t, cs, tool, args)
		if strings.Contains(out, "go-build") || strings.Contains(out, "/.cache/") {
			t.Errorf("%s lists generated cgo files:\n%s", tool, out)
		}
		mustHave(t, tool, out, "cgb/cg.go")
	}
}

// N6: an absurdly long identifier is refused; an answer is capped with a note saying how much was left out.
func TestRenameRefusesHugeIdentifier(t *testing.T) {
	cs, root := session(t, nil)
	before := readFile(t, root, "shapes/shapes.go")
	out := call(t, cs, "rename_symbol", map[string]any{"name": "shapes.Scale", "new_name": strings.Repeat("a", 100000), "apply": false})
	mustHave(t, "huge", out, "new_name is 100000 characters long", "over 255")
	if len(out) > 2000 {
		t.Errorf("the refusal must be short, got %d bytes", len(out))
	}
	out = call(t, cs, "rename_symbol", map[string]any{"name": "shapes.Scale", "new_name": "S" + strings.Repeat("a", 254), "apply": false})
	if strings.Contains(out, "characters long") {
		t.Errorf("255 characters is allowed:\n%s", out)
	}
	if readFile(t, root, "shapes/shapes.go") != before {
		t.Error("nothing may be written")
	}
}

// N7: values the JSON decoder cannot fit into an int get a sentence naming the parameter, not Go internals.
func TestHugeIntegersGetFriendlyMessage(t *testing.T) {
	cs, _ := session(t, nil)
	for name, c := range map[string]struct {
		tool string
		args map[string]any
		want string
	}{
		"line 2^63":      {"references", map[string]any{"file_path": "shapes/shapes.go", "line": 9223372036854775807, "column": 1}, "line must be a whole number of reasonable size"},
		"column 1e30":    {"references", map[string]any{"file_path": "shapes/shapes.go", "line": 3, "column": 1e30}, "column must be a whole number of reasonable size"},
		"max_new_errors": {"edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "x", "new_string": "y", "max_new_errors": 1e30}, "max_new_errors must be a whole number of reasonable size"},
	} {
		out := call(t, cs, c.tool, c.args)
		mustHave(t, name, out, c.want, "Usage: "+c.tool+"(")
		if strings.Contains(out, "Go struct field") || strings.Contains(out, "json:") {
			t.Errorf("%s: Go decoder text leaked:\n%s", name, out)
		}
	}
}

// N8: gopls and type-checker messages show workspace-relative paths (DuplicateMethod used to print the
// absolute path of the other declaration).
func TestDiagnosticMessagesAreWorkspaceRelative(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "dm/dm.go", "package dm\n\ntype Item struct{}\n\nfunc (i *Item) get() int { return 1 }\n\nfunc (i *Item) Get() int { return 2 }\n")
	out := call(t, cs, "rename_symbol", map[string]any{"name": "dm.Item.Get", "new_name": "get", "apply": false})
	mustHave(t, "duplicate", out, "dm/dm.go:5:16", "would conflict with this method")
	if strings.Contains(out, root) {
		t.Errorf("absolute workspace path leaked:\n%s", out)
	}
}

// N9a: a use outside the workspace root is listed relative to it ("../sibling/s.go"), not as an absolute path.
func TestEditSymbolDeleteListsOutsideUsesRelative(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Dir(root)
	copyTree(t, "../../testdata/fixture", root)
	writeFile(t, parent, "go.work", "go 1.24\n\nuse (\n\t./"+filepath.Base(root)+"\n\t./sibling\n)\n")
	writeFile(t, parent, "sibling/go.mod", "module example.com/sibling\n\ngo 1.24\n\nrequire example.com/fx v0.0.0\n")
	writeFile(t, parent, "sibling/s.go", "package sibling\n\nimport \"example.com/fx/shapes\"\n\nvar _ = shapes.Scale\n")
	cs, _ := sessionAt(t, root)
	out := call(t, cs, "edit_symbol", map[string]any{"action": "delete", "name": "shapes.Scale", "apply": false})
	mustHave(t, "delete", out, "still used", "../sibling/s.go:")
	if strings.Contains(out, parent) {
		t.Errorf("absolute path leaked:\n%s", out)
	}
}

// N9d: the broken-configuration warning is for results it can explain; a mistake in the arguments or an
// old_string that is not in the file is not one of them.
func TestConfigWarningNotOnArgumentMistakes(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "go.work", "go 1.24\n\nuse (\n\t.\n\t./gone\n)\n")
	const warn = "Go configuration of this workspace is broken"
	mustHave(t, "search", call(t, cs, "search_symbol", map[string]any{"query": "Scale"}), warn)
	for name, c := range map[string]struct {
		tool string
		args map[string]any
	}{
		"old_string not found": {"edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "no such text", "new_string": "x", "apply": false}},
		"bad identifier":       {"rename_symbol", map[string]any{"name": "shapes.Scale", "new_name": "not valid", "apply": false}},
		"negative max":         {"edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "x", "new_string": "y", "max_new_errors": -5}},
	} {
		if out := call(t, cs, c.tool, c.args); strings.Contains(out, warn) {
			t.Errorf("%s: the warning has nothing to do with this mistake:\n%s", name, out)
		}
	}
	// A symbol lookup that finds nothing may be caused by the broken configuration: it keeps the warning.
	mustHave(t, "not found", call(t, cs, "rename_symbol", map[string]any{"name": "NoSuchThing", "new_name": "X", "apply": false}), warn)
	if n := strings.Count(call(t, cs, "search_symbol", map[string]any{"query": "Scale"}), warn); n != 1 {
		t.Errorf("the warning is shown once per result, got %d", n)
	}
}
