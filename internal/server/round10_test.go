package server

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// goWorkFixture adds a second module to the workspace (a go.work `use`) whose packages call each other
// and are called from the root module.
func goWorkFixture(t *testing.T, root string) {
	t.Helper()
	writeFile(t, root, "go.work", "go 1.24\n\nuse (\n\t.\n\t./nested\n)\n")
	writeFile(t, root, "nested/go.mod", "module example.com/nested\n\ngo 1.24\n")
	writeFile(t, root, "nested/sub/sub.go", "package sub\n\nfunc Sub() int { return 7 }\n")
	writeFile(t, root, "nested/n.go", "package nested\n\nimport \"example.com/nested/sub\"\n\nfunc Nest() int { return sub.Sub() }\n")
	writeFile(t, root, "nested/other/o.go", "package other\n\nimport \"example.com/nested/sub\"\n\nfunc O() int { return sub.Sub() + 1 }\n")
	writeFile(t, root, "app/nest.go", "package app\n\nimport \"example.com/nested\"\n\nfunc UseNest() int { return nested.Nest() }\n")
}

func TestGoWorkMemberModuleIsTypeChecked(t *testing.T) {
	cs, root := session(t, nil)
	goWorkFixture(t, root)
	for _, deps := range []bool{true} {
		out := call(t, cs, "edit", map[string]any{"file_path": "nested/sub/sub.go", "old_string": "func Sub()", "new_string": "func Sub(k int)", "apply": false, "include_dependents": deps})
		if !strings.Contains(out, "2 new error(s)") || !strings.Contains(out, "nested/n.go") || !strings.Contains(out, "nested/other/o.go") {
			t.Errorf("include_dependents=%v: callers in the member module must be checked:\n%s", deps, out)
		}
	}
	out := call(t, cs, "edit", map[string]any{"file_path": "nested/n.go", "old_string": "Nest()", "new_string": "Nest(x int)", "apply": false, "include_dependents": true})
	if !strings.Contains(out, "app/nest.go") || !strings.Contains(out, "1 new error(s)") {
		t.Errorf("a caller in the root module must be checked:\n%s", out)
	}
}

func TestGoWorkMemberModuleSymbolsMove(t *testing.T) {
	cs, root := session(t, nil)
	goWorkFixture(t, root)
	out := call(t, cs, "move", map[string]any{"name": "sub.Sub", "to_file": "nested/sub/oo.go", "apply": false})
	if strings.Contains(out, "not part of a package") || !strings.Contains(out, "nested/sub/oo.go") {
		t.Errorf("moving within a member module:\n%s", out)
	}
	// Into the root module: the caller in the root module (found through export data) is rewritten.
	out = call(t, cs, "move", map[string]any{"name": "nested.Nest", "to_file": "app/nest2.go", "apply": false})
	if strings.Contains(out, "not part of a package") || !strings.Contains(out, "0 new error(s)") || !strings.Contains(out, "app/nest.go") {
		t.Errorf("moving out of a member module:\n%s", out)
	}
}

func TestNoPkgGoDevLinksForWorkspaceModules(t *testing.T) {
	cs, root := session(t, nil)
	goWorkFixture(t, root)
	// line 5: func UseNest() int { return nested.Nest() }; "Nest" starts at column 38
	out := call(t, cs, "hover", map[string]any{"file_path": "app/nest.go", "line": 5, "column": 38})
	if !strings.Contains(out, "Nest") || strings.Contains(out, "pkg.go.dev") {
		t.Errorf("hover on a workspace module symbol must not link pkg.go.dev:\n%s", out)
	}
	out = call(t, cs, "symbol_info", map[string]any{"name": "shapes.Scale"})
	if strings.Contains(out, "pkg.go.dev") {
		t.Errorf("symbol_info on the main module must not link pkg.go.dev:\n%s", out)
	}
}

func TestCgoReplaceKeepsPreambleAndExportedFuncsAreProtected(t *testing.T) {
	skipWithoutCgo(t)
	cs, root := session(t, nil)
	writeFile(t, root, "cg2/cg.go", "package cg2\n\n/*\nstatic int add(int a, int b) { return a + b; }\n*/\nimport \"C\"\n\nfunc Add(a, b int) int { return int(C.add(C.int(a), C.int(b))) }\n")
	out := call(t, cs, "edit_symbol", map[string]any{"action": "replace", "name": "cg2.Add", "source": "func Add(a, b int) int { return a + b }", "apply": false})
	if strings.Contains(out, `-import "C"`) || strings.Contains(out, "-static int add") {
		t.Errorf("replace must keep import \"C\" and the preamble:\n%s", out)
	}
	writeFile(t, root, "cx/cx.go", "package cx\n\n/*\nextern int goAdd(int, int);\nstatic int callGo(int a, int b) { return goAdd(a, b); }\n*/\nimport \"C\"\n\n//export goAdd\nfunc goAdd(a, b C.int) C.int { return a + b }\n\nfunc Run() int { return int(C.callGo(1, 2)) }\n")
	out = call(t, cs, "change_signature", map[string]any{"name": "cx.goAdd", "add": []any{map[string]any{"name": "c", "type": "int", "value": "0"}}, "apply": false})
	if !strings.Contains(out, "exported to C (//export goAdd)") || strings.Contains(out, "go-build") || strings.Contains(out, "outside the workspace") {
		t.Errorf("change_signature on //export:\n%s", out)
	}
	out = call(t, cs, "edit_symbol", map[string]any{"action": "delete", "name": "cx.goAdd", "apply": false})
	if !strings.Contains(out, "exported to C") || strings.Contains(out, "go-build") {
		t.Errorf("delete of //export func:\n%s", out)
	}
}

func TestFriendlyArgumentErrors(t *testing.T) {
	cs, _ := session(t, nil)
	for _, c := range []struct {
		tool string
		args map[string]any
		want []string
	}{
		{"hover", map[string]any{"file_path": "shapes/shapes.go", "line": "5", "column": 1}, []string{`line must be an integer (got string "5")`, "Usage: hover(file_path, line, column)"}},
		{"rename_symbol", map[string]any{"name": "shapes.Scale", "new_name": 5}, []string{"new_name must be a string (got integer 5)"}},
		{"edit", map[string]any{"file_path": "a.go", "old_string": "a", "new_string": "b", "apply": "yes"}, []string{`apply must be a boolean`, `(got string "yes")`}},
		{"edit", map[string]any{"file_path": "a.go", "old_string": "a", "new_string": "b", "max_new_errors": "x"}, []string{"max_new_errors must be an integer"}},
		{"change_signature", map[string]any{"name": "shapes.Scale", "add": map[string]any{"name": "x"}}, []string{"add must be an array (got object)"}},
		{"change_signature", map[string]any{"name": "shapes.Scale", "add": []any{"x"}}, []string{"each item of add must be an object"}},
		{"change_signature", map[string]any{"name": "shapes.Scale", "add": []any{map[string]any{"name": "x", "type": "int", "default": "0"}}}, []string{`unknown field default in an item of add`, `use "value"`}},
		{"references", map[string]any{"file_path": nil}, []string{"references(file_path, line, column, [include_declaration])"}},
	} {
		out := call(t, cs, c.tool, c.args)
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s %v: want %q in:\n%s", c.tool, c.args, w, out)
			}
		}
		if strings.Contains(out, "validating") || strings.Contains(out, "posIn") {
			t.Errorf("%s: raw validator text leaked:\n%s", c.tool, out)
		}
	}
}

func TestFriendlyParseErrors(t *testing.T) {
	cs, root := session(t, nil)
	for _, c := range []struct {
		add  map[string]any
		want string
	}{
		{map[string]any{"name": "1x", "type": "int", "value": "0"}, `"1x" is not a valid Go parameter name`},
		{map[string]any{"name": "x", "type": "int", "value": "1 +"}, `value "1 +" of added parameter x is not a valid Go expression`},
	} {
		out := call(t, cs, "change_signature", map[string]any{"name": "shapes.Scale", "add": []any{c.add}, "apply": false})
		if !strings.Contains(out, c.want) || strings.Contains(out, root) {
			t.Errorf("want %q in:\n%s", c.want, out)
		}
	}
	out := call(t, cs, "edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "Scale", "new_string": "Sc ale (", "replace_all": true, "apply": false})
	if !strings.Contains(out, "not valid Go: shapes/shapes.go:") || strings.Contains(out, root) {
		t.Errorf("syntax errors name the file relative to the workspace:\n%s", out)
	}
}

func TestSmallMessages(t *testing.T) {
	cs, _ := session(t, nil)
	out := call(t, cs, "edit_symbol", map[string]any{"action": "insert", "file_path": "shapes/shapes.go", "source": "func Z() {}", "position": "nonsense"})
	if !strings.Contains(out, `unknown position "nonsense"`) || !strings.Contains(out, "after") || !strings.Contains(out, "end") {
		t.Errorf("unknown position:\n%s", out)
	}
	out = call(t, cs, "symbol_info", map[string]any{"name": "."})
	if strings.Contains(out, "  itself") || strings.Contains(out, "look up  ") {
		t.Errorf("double space for an empty name:\n%s", out)
	}
	out = call(t, cs, "change_signature", map[string]any{"name": "shapes.Shape", "remove": []any{"x"}})
	if strings.Contains(out, "a interface") {
		t.Errorf("article:\n%s", out)
	}
	out = call(t, cs, "apply_edit", map[string]any{"id": "zzzz"})
	if strings.Contains(out, "(pending: )") {
		t.Errorf("empty pending list:\n%s", out)
	}
	long := strings.Repeat("q", 400)
	out = call(t, cs, "search_symbol", map[string]any{"query": long})
	if len(out) > 300 {
		t.Errorf("long query echoed in full (%d bytes)", len(out))
	}
}

func TestPreviewWarnsWhenMaxNewErrorsWouldRefuse(t *testing.T) {
	cs, _ := session(t, nil)
	out := call(t, cs, "edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": scaleSig, "new_string": "func Scale(s Shape, k float64)", "apply": false})
	if !strings.Contains(out, "would be refused") || !strings.Contains(out, "max_new_errors=") {
		t.Errorf("a preview with errors above the limit must say so:\n%s", out)
	}
	out = call(t, cs, "edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": scaleSig, "new_string": "func Scale(s Shape, k float64)", "apply": false, "max_new_errors": -1})
	if strings.Contains(out, "would be refused") {
		t.Errorf("no warning without a limit:\n%s", out)
	}
}

func TestDirectoryMoveNoSpuriousBuildConstraintNote(t *testing.T) {
	cs, _ := session(t, nil)
	out := call(t, cs, "move", map[string]any{"file_path": "shapes", "to_file": "geo", "apply": false})
	if strings.Contains(out, "build constraint") {
		t.Errorf("no file has a build constraint:\n%s", out)
	}
}

func TestQuickFixChoiceHeldBackReason(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "qf/q.go", "package qf\n\ntype I interface{ M() }\n\ntype Impl struct{}\n\nvar _ I = Impl{}\n\nfunc B() int { return undefinedThing }\n")
	out := call(t, cs, "quick_fix", map[string]any{"file_path": "qf/q.go", "choice": "Declare", "apply": false})
	if strings.Contains(out, "not applied: strict") || !strings.Contains(out, "held back") {
		t.Errorf("a diagnostic whose fix matched the choice must not read as unmatched:\n%s", out)
	}
}

func TestDiagnosticsOutsideWorkspaceExplained(t *testing.T) {
	cs, _ := session(t, nil)
	out := call(t, cs, "diagnostics", map[string]any{"file_path": filepath.Join(runtime.GOROOT(), "src", "fmt", "print.go")})
	if !strings.Contains(out, "diagnostics only cover files of the workspace's own modules") {
		t.Errorf("diagnostics outside the workspace:\n%s", out)
	}
}

func TestRenameLeftoverMentionsStayInTheSymbolsPackage(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "sn/sn.go", "package sn\n\nfunc g(x int) int { return x }\n\n// g does g things.\nvar s = \"say g now\"\n\nfunc Use() int { return g(1) }\n")
	writeFile(t, root, "gen/g.go", "package gen\n\n// Map maps T to U; T is the input.\nfunc Map[T, U any](xs []T, f func(T) U) []U { return nil }\n\nvar s = \"xs out T\"\n")
	out := call(t, cs, "rename_symbol", map[string]any{"name": "sn.g", "new_name": "gg", "apply": false})
	if !strings.Contains(out, `sn/sn.go:6`) {
		t.Errorf("a whole-word mention in a string of the same package is reported for a short name:\n%s", out)
	}
	writeFile(t, root, "sn/sn.go", "package sn\n\ntype T struct{ V int }\n\nvar _ T\n")
	out = call(t, cs, "rename_symbol", map[string]any{"name": "sn.T", "new_name": "U", "apply": false})
	if strings.Contains(out, "gen/g.go") {
		t.Errorf("type parameters of another package are not mentions:\n%s", out)
	}
}

func TestOrganizeImportsDropsStackedAndTrailingHeadings(t *testing.T) {
	cs, root := session(t, nil)
	// Not in shapes: app imports shapes, so a shapes file importing app is an import cycle, and a package that
	// does not load is held as a preview (NOT CHECKED) instead of written.
	writeFile(t, root, "orph/orph.go", "package orph\n\nimport (\n\t// Standard library\n\t\"fmt\"\n\t\"os\"\n\n\t\"sort\"\n\t// Another heading\n\n\t// Local packages\n\t\"example.com/fx/app\"\n\t// Trailing heading\n)\n\nfunc O1() { fmt.Println(os.Args) }\nfunc O2() { _ = app.Total }\n")
	writeFile(t, root, "orph/orph2.go", "package orph\n\nimport (\n\t\"os\" // used\n\t\"strings\"\n\t/* block heading */\n)\n\nfunc P1() { _ = os.Args }\n")
	call(t, cs, "quick_fix", map[string]any{"file_path": "orph/orph.go", "organize_imports": true, "max_new_errors": -1})
	call(t, cs, "quick_fix", map[string]any{"file_path": "orph/orph2.go", "organize_imports": true, "max_new_errors": -1})
	got := readFile(t, root, "orph/orph.go")
	if strings.Contains(got, "Another heading") || strings.Contains(got, "Trailing heading") || strings.Contains(got, "\n\n\n") || !strings.Contains(got, "// Local packages") {
		t.Errorf("orph.go:\n%s", got)
	}
	got = readFile(t, root, "orph/orph2.go")
	if strings.Contains(got, "block heading") || strings.Contains(got, "*/)") {
		t.Errorf("orph2.go:\n%s", got)
	}
}
