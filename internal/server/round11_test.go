package server

import (
	"strings"
	"testing"
)

func mustHave(t *testing.T, label, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("%s: want %q in:\n%s", label, w, out)
		}
	}
}

func TestWriteToolsAreCaseSensitive(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "gen/g.go", "package gen\n\nfunc Helper() int { return 1 }\n")
	writeFile(t, root, "gen/types.go", "package gen\n\ntype Gen struct{ box int }\n\nfunc Sub() int { return 2 }\n")
	for _, name := range []string{"gen", "sub", "gen.box"} {
		out := call(t, cs, "rename_symbol", map[string]any{"name": name, "new_name": "Other", "apply": false})
		if strings.Contains(out, "edit ") || strings.Contains(out, "Applied") {
			t.Errorf("rename %q must not guess a case-insensitive match:\n%s", name, out)
		}
	}
	out := call(t, cs, "rename_symbol", map[string]any{"name": "sub", "new_name": "Other", "apply": false})
	mustHave(t, "sub", out, "Did you mean `Sub`?", "case-sensitive")
	out = call(t, cs, "edit_symbol", map[string]any{"name": "sub", "action": "delete", "apply": false})
	mustHave(t, "edit_symbol", out, "Did you mean `Sub`?")
	out = call(t, cs, "rename_symbol", map[string]any{"name": "gen.Gen", "new_name": "Gen2", "apply": false})
	if strings.Contains(out, "Did you mean") {
		t.Errorf("exact name must still work:\n%s", out)
	}
	out = call(t, cs, "symbol_info", map[string]any{"name": "sub"})
	mustHave(t, "symbol_info", out, "names are case-sensitive")
}

// noWorkFixture: two modules in sibling directories, joined by a replace directive, with no go.work.
func noWorkFixture(t *testing.T, root string) {
	t.Helper()
	writeFile(t, root, "outdep/go.mod", "module example.com/outdep\n\ngo 1.24\n")
	writeFile(t, root, "outdep/o.go", "package outdep\n\nfunc Hello() string { return \"hi\" }\n")
	writeFile(t, root, "appmod/go.mod", "module example.com/appmod\n\ngo 1.24\n\nrequire example.com/outdep v0.0.0\n\nreplace example.com/outdep => ../outdep\n")
	writeFile(t, root, "appmod/main.go", "package main\n\nimport \"example.com/outdep\"\n\nfunc main() { _ = outdep.Hello() }\n")
}

func TestSiblingModuleWithoutGoWorkIsTypeChecked(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "go.work", "go 1.24\n\nuse .\n")
	noWorkFixture(t, root)
	// appmod sits in the tree as an uncovered module: its callers must be type-checked.
	out := call(t, cs, "rename_symbol", map[string]any{"name": "outdep.Hello", "new_name": "Greet", "apply": false})
	mustHave(t, "rename", out, "NOT rewritten")
}

func TestMoveOutOfUncoveredModuleExplains(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "go.work", "go 1.24\n\nuse .\n")
	noWorkFixture(t, root)
	out := call(t, cs, "move", map[string]any{"name": "outdep.Hello", "to_file": "shapes/hello.go", "apply": false})
	if strings.Contains(out, "not part of a package in this workspace's module") {
		t.Errorf("unhelpful message:\n%s", out)
	}
	mustHave(t, "move", out, "go.work")
}

func TestBrokenGoModIsReported(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "go.work", "go 1.24\n\nuse (\n\t.\n\t./missing\n)\n")
	out := call(t, cs, "search_symbol", map[string]any{"query": "Scale"})
	mustHave(t, "search_symbol", out, "Go configuration of this workspace is broken")
}

func TestQuickFixCodeMismatchAndInsertMissingArg(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "shapes/qf.go", "package shapes\n\nfunc QF() {\n\tx := 1\n}\n")
	out := call(t, cs, "quick_fix", map[string]any{"file_path": "shapes/qf.go", "code": "ZZZ", "apply": false})
	mustHave(t, "quick_fix", out, "No diagnostics with code ZZZ")
	if strings.Contains(out, "Not fixed:") {
		t.Errorf("no Applied/Not fixed tail when nothing was targeted:\n%s", out)
	}
}

func TestChangeSignatureNamesTheIncompleteItem(t *testing.T) {
	cs, _ := session(t, nil)
	out := call(t, cs, "change_signature", map[string]any{"name": "shapes.Scale", "add": []any{map[string]any{"name": "a", "type": "int", "value": "1"}, map[string]any{"name": "b", "type": "int"}}, "apply": false})
	mustHave(t, "change_signature", out, "add[1] is missing value")
}

func TestOrganizeImportsRound11Comments(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "shapes/oc.go", "package shapes\n\n// Deprecated: old\n// import \"strings\"\nimport (\n\t\"os\" // used\n\t\"strings\" // unused\n)\n\nfunc OC() { _ = os.Args }\n")
	call(t, cs, "quick_fix", map[string]any{"file_path": "shapes/oc.go", "organize_imports": true, "max_new_errors": -1})
	got := readFile(t, root, "shapes/oc.go")
	if strings.Contains(got, "unused") || strings.Contains(got, "\t\"strings\"") {
		t.Errorf("leftover of the removed import:\n%s", got)
	}
}
