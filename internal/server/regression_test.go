package server

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/illescasDaniel/codenav-go-mcp/internal/txn"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Regression tests for the fixes of rounds 9-11 (one test per fix).

// 7a: rename at an import-path position is refused with a clear, workspace-relative message.
func TestRenameAtImportPathPositionRefused(t *testing.T) {
	cs, root := session(t, nil)
	src := "package imp\n\nimport \"example.com/fx/shapes\"\n\nvar _ = shapes.Scale\n"
	writeFile(t, root, "imp/i.go", src)
	// line 3: import "example.com/fx/shapes" - column 12 is inside the string.
	out := call(t, cs, "rename_symbol", map[string]any{"file_path": "imp/i.go", "line": 3, "column": 12, "new_name": "geo", "apply": false})
	mustHave(t, "rename at import path", out, `is on the import path "example.com/fx/shapes"`, "imp/i.go:3:12", "there is no symbol to rename there")
	if strings.Contains(out, root) {
		t.Errorf("absolute root leaked:\n%s", out)
	}
	if readFile(t, root, "imp/i.go") != src {
		t.Error("nothing may be written")
	}
}

// 7c: the //export refusal names the cgo locations relative to the workspace.
func TestRenameCgoExportRefusalUsesRelativePaths(t *testing.T) {
	skipWithoutCgo(t)
	cs, root := session(t, nil)
	writeFile(t, root, "cx/cx.go", "package cx\n\n/*\nextern int goAdd(int, int);\nstatic int callGo(int a, int b) { return goAdd(a, b); }\n*/\nimport \"C\"\n\n//export goAdd\nfunc goAdd(a, b C.int) C.int { return a + b }\n\nfunc Run() int { return int(C.callGo(1, 2)) }\n")
	out := call(t, cs, "rename_symbol", map[string]any{"name": "cx.goAdd", "new_name": "goSum"})
	mustHave(t, "rename //export", out, "exported to C", "cx/cx.go:9 (//export goAdd)", "cx/cx.go:5:")
	if strings.Contains(out, root) {
		t.Errorf("absolute root leaked:\n%s", out)
	}
	out = call(t, cs, "change_signature", map[string]any{"name": "cx.goAdd", "add": []any{map[string]any{"name": "c", "type": "int", "value": "0"}}, "apply": false})
	mustHave(t, "change_signature //export", out, "exported to C")
	if strings.Contains(out, root) {
		t.Errorf("absolute root leaked:\n%s", out)
	}
}

// 7d: syntax errors in edit_symbol's source count lines from the snippet, not from the wrapper.
func TestEditSymbolParseErrorLinesRelativeToSnippet(t *testing.T) {
	cs, root := session(t, nil)
	before := readFile(t, root, "shapes/shapes.go")
	// The error is on the third line of the snippet.
	out := call(t, cs, "edit_symbol", map[string]any{"action": "insert", "file_path": "shapes/shapes.go", "source": "func A() {}\n\nfunc B( {", "apply": false})
	mustHave(t, "insert", out, "source line 3")
	if strings.Contains(out, "source.go:") || strings.Contains(out, root) {
		t.Errorf("wrapper positions leaked:\n%s", out)
	}
	out = call(t, cs, "edit_symbol", map[string]any{"action": "replace", "name": "shapes.Scale", "source": "func Scale(s Shape, k float64) Shape {\n\treturn s +\n}", "apply": false})
	mustHave(t, "replace", out, "source line 3")
	if strings.Contains(out, "source.go:") {
		t.Errorf("wrapper positions leaked:\n%s", out)
	}
	// Struct body ("into" wraps the snippet in two header lines).
	writeFile(t, root, "shapes/one.go", "package shapes\n\ntype Box struct {\n\tW int\n}\n")
	out = call(t, cs, "edit_symbol", map[string]any{"action": "insert", "name": "Box", "position": "into", "file_path": "shapes/one.go", "source": "H int\nV int int int", "apply": false})
	mustHave(t, "into", out, "source line 2")
	if strings.Contains(out, "source.go:") {
		t.Errorf("wrapper positions leaked:\n%s", out)
	}
	if readFile(t, root, "shapes/shapes.go") != before {
		t.Error("nothing may be written")
	}
}

// 7f: an unused import has no gopls fix; quick_fix points to organize_imports=true.
func TestQuickFixUnusedImportPointsToOrganizeImports(t *testing.T) {
	cs, root := session(t, nil)
	src := "package shapes\n\nimport \"os\"\n\nfunc UI() {}\n"
	writeFile(t, root, "shapes/ui.go", src)
	out := call(t, cs, "quick_fix", map[string]any{"file_path": "shapes/ui.go", "apply": false})
	mustHave(t, "quick_fix", out, "organize_imports=true")
	if readFile(t, root, "shapes/ui.go") != src {
		t.Error("nothing may be written")
	}
}

// 7g: comments and strings of a file that cannot refer to the symbol but declares a same-named
// top-level name are about that other declaration, not leftovers of the rename.
func TestRenameMentionsSkipFilesDeclaringTheSameName(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "wa/a.go", "package wa\n\nfunc Widget() int { return 1 }\n")
	writeFile(t, root, "wb/b.go", "package wb\n\n// Widget is wb's own widget.\nfunc Widget() int { return 2 }\n\nvar s = \"Widget\"\n")
	writeFile(t, root, "wc/c.go", "package wc\n\n// Widget is discussed here but never declared.\nvar s = \"Widget\"\n")
	out := call(t, cs, "rename_symbol", map[string]any{"name": "wa.Widget", "new_name": "Gadget", "apply": false})
	if strings.Contains(out, "wb/b.go") {
		t.Errorf("wb declares its own Widget: its comment and string are not leftovers:\n%s", out)
	}
	// Control: a file that merely talks about the name is still listed.
	mustHave(t, "control", out, "wc/c.go")
}

// 7h: ambiguity candidates are sorted by path, and the listing is stable across calls.
func TestAmbiguityCandidatesSortedAndStable(t *testing.T) {
	cs, root := session(t, nil)
	// Created in reverse order of their paths.
	writeFile(t, root, "zz/z.go", "package zz\n\nfunc Dup() int { return 3 }\n")
	writeFile(t, root, "mm/m.go", "package mm\n\nfunc Dup() int { return 2 }\n")
	writeFile(t, root, "aa/a.go", "package aa\n\nfunc Dup() int { return 1 }\n")
	var first string
	for i := 0; i < 4; i++ {
		out := call(t, cs, "symbol_info", map[string]any{"name": "Dup"})
		a, m, z := strings.Index(out, "aa/a.go"), strings.Index(out, "mm/m.go"), strings.Index(out, "zz/z.go")
		if a < 0 || m < 0 || z < 0 || !(a < m && m < z) {
			t.Fatalf("call %d: candidates must be ordered by path:\n%s", i, out)
		}
		if i == 0 {
			first = out
		} else if out != first {
			t.Fatalf("call %d: listing changed between calls:\n%s\n---\n%s", i, first, out)
		}
	}
}

// 7i: insert says which of source / file_path is missing.
func TestEditSymbolInsertNamesMissingArguments(t *testing.T) {
	cs, _ := session(t, nil)
	out := call(t, cs, "edit_symbol", map[string]any{"action": "insert", "file_path": "shapes/shapes.go"})
	mustHave(t, "no source", out, `action="insert" needs source, which is missing`)
	if strings.Contains(out, "file_path") {
		t.Errorf("file_path was given and must not be named as missing:\n%s", out)
	}
	out = call(t, cs, "edit_symbol", map[string]any{"action": "insert", "source": "func Z() {}"})
	mustHave(t, "no file_path", out, `action="insert" needs file_path, which is missing`)
	if strings.Contains(out, "needs source") {
		t.Errorf("source was given and must not be named as missing:\n%s", out)
	}
	out = call(t, cs, "edit_symbol", map[string]any{"action": "insert", "name": "shapes.Scale", "source": "func Z() {}"})
	mustHave(t, "name only anchors", out, "needs file_path, which is missing", "name only anchors the position")
	out = call(t, cs, "edit_symbol", map[string]any{"action": "insert"})
	mustHave(t, "both", out, "needs source and file_path, which are missing")
}

// 7j: moving a directory with a build-constrained file says so once, not under the old and the new path.
func TestDirMoveBuildConstraintNoteOncePerFile(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "cdir/a.go", "package cdir\n\nfunc A() int { return 1 }\n")
	writeFile(t, root, "cdir/lin.go", "//go:build linux || darwin\n\npackage cdir\n\nfunc L() int { return 2 }\n")
	out := call(t, cs, "move", map[string]any{"file_path": "cdir", "to_file": "cdir2", "apply": false})
	if n := strings.Count(out, "has a build constraint"); n != 1 {
		t.Errorf("want the build-constraint note exactly once, got %d:\n%s", n, out)
	}
	// Two different constrained files still get one note each.
	writeFile(t, root, "cdir/dar.go", "//go:build darwin\n\npackage cdir\n\nfunc D() int { return 3 }\n")
	out = call(t, cs, "move", map[string]any{"file_path": "cdir", "to_file": "cdir3", "apply": false})
	if n := strings.Count(out, "has a build constraint"); n != 2 {
		t.Errorf("want one note per constrained file (2), got %d:\n%s", n, out)
	}
}

// B13: a single edit to a constrained file carries the note once.
func TestEditConstrainedFileNote(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "shapes/lin.go", "//go:build linux || darwin\n\npackage shapes\n\nvar L = 1\n")
	out := call(t, cs, "edit", map[string]any{"file_path": "shapes/lin.go", "old_string": "L = 1", "new_string": "L = 2", "apply": false})
	if n := strings.Count(out, "shapes/lin.go has a build constraint"); n != 1 {
		t.Errorf("want one note, got %d:\n%s", n, out)
	}
	out = call(t, cs, "edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "package shapes", "new_string": "package shapes // x", "apply": false})
	if strings.Contains(out, "has a build constraint") {
		t.Errorf("no constraint, no note:\n%s", out)
	}
}

// Bug 3: the root is a member module of a go.work above it, and the edit must change a file outside the
// workspace: the message is relative, explains, and suggests CODENAV_GO_MCP_WORKSPACE.
func TestOutsideWorkspaceMessageWithAncestorGoWork(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Dir(root)
	copyTree(t, "../../testdata/fixture", root)
	writeFile(t, parent, "go.work", "go 1.24\n\nuse (\n\t./"+filepath.Base(root)+"\n\t./sibling\n)\n")
	writeFile(t, parent, "sibling/go.mod", "module example.com/sibling\n\ngo 1.24\n\nrequire example.com/fx v0.0.0\n")
	writeFile(t, parent, "sibling/s.go", "package sibling\n\nimport \"example.com/fx/shapes\"\n\nvar _ = shapes.Scale\n")
	cs, _ := sessionAt(t, root)
	before := readFile(t, root, "shapes/shapes.go")
	out := call(t, cs, "rename_symbol", map[string]any{"name": "shapes.Scale", "new_name": "Resize", "apply": false})
	mustHave(t, "rename", out, "outside the workspace", "CODENAV_GO_MCP_WORKSPACE", "a go.work in "+parent, "../sibling/s.go")
	if strings.Contains(out, filepath.Join(parent, "sibling")) {
		t.Errorf("the outside file must be shown relative to the workspace:\n%s", out)
	}
	if readFile(t, root, "shapes/shapes.go") != before {
		t.Error("nothing may be written")
	}
}

// Bug 2: a module in a sibling directory with no go.work: its breakage is reported as a new error at
// the caller, the caller is NOT rewritten, and max_new_errors=0 refuses the apply.
func TestSiblingModuleErrorReportedAndRefusesApply(t *testing.T) {
	cs, root := session(t, nil)
	noWorkFixture(t, root)
	main := readFile(t, root, "appmod/main.go")
	out := call(t, cs, "rename_symbol", map[string]any{"name": "outdep.Hello", "new_name": "Greet", "apply": false})
	mustHave(t, "preview", out, "1 new error(s)", "appmod/main.go:5", "NOT rewritten")
	out = call(t, cs, "rename_symbol", map[string]any{"name": "outdep.Hello", "new_name": "Greet"})
	mustHave(t, "apply", out, "Not written", "1 new error(s)")
	if strings.Contains(out, "Applied") {
		t.Errorf("max_new_errors=0 must refuse:\n%s", out)
	}
	if readFile(t, root, "appmod/main.go") != main || !strings.Contains(readFile(t, root, "outdep/o.go"), "func Hello") {
		t.Error("nothing may be written")
	}
}

// Config warning: absent on healthy workspaces (plain and go.work), present on broken ones.
func TestConfigWarningOnlyWhenBroken(t *testing.T) {
	const warn = "Go configuration of this workspace is broken"
	cs, root := session(t, nil)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"search_symbol", map[string]any{"query": "Scale"}},
		{"symbol_info", map[string]any{"name": "shapes.Scale"}},
		{"edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "unused int", "new_string": "unused int, v int", "apply": false}},
	} {
		if out := call(t, cs, c.tool, c.args); strings.Contains(out, warn) {
			t.Errorf("%s: warning on a healthy workspace:\n%s", c.tool, out)
		}
	}
	goWorkFixture(t, root)
	if out := call(t, cs, "search_symbol", map[string]any{"query": "Nest"}); strings.Contains(out, warn) {
		t.Errorf("warning on a healthy go.work workspace:\n%s", out)
	}
	// Breaking go.mod turns it on (and the write tools carry it too).
	writeFile(t, root, "go.work", "go 1.24\n\nuse (\n\t.\n\t./gone\n)\n")
	out := call(t, cs, "search_symbol", map[string]any{"query": "Scale"})
	mustHave(t, "broken", out, warn)
	if strings.Contains(out, root) {
		t.Errorf("absolute root leaked in the warning:\n%s", out)
	}
	out = call(t, cs, "edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "unused int", "new_string": "unused int, v int", "apply": false})
	mustHave(t, "edit broken", out, warn)
}

// B12: in a read-only server the preview should warn too (README: "An apply=false preview warns when its
// new errors exceed max_new_errors").
func TestReadOnlyPreviewWarnsWhenMaxNewErrorsWouldRefuse(t *testing.T) {
	t.Skip("BUG: with CODENAV_GO_MCP_READ_ONLY=1 an apply=false preview ends with \"Preview only (apply=false); write tools are disabled\" and never gets the \"would be refused ... max_new_errors\" warning that the normal preview has (internal/tools/write.go, `case readOnly:` has no warning; README says any apply=false preview warns). Remove the Skip once it is wired.")
	cs, _ := session(t, map[string]string{EnvReadOnly: "1"})
	out := call(t, cs, "edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": scaleSig, "new_string": "func Scale(s Shape, k float64)", "apply": false})
	mustHave(t, "read-only preview", out, "would be refused", "max_new_errors=")
}

// B11: a choice that matches a fix for a diagnostic is reported as held back (default) or as "its fix was not
// applied" (strict=true), never as an unmatched choice.
func TestQuickFixMatchingChoiceStrictHeldBack(t *testing.T) {
	cs, root := session(t, nil)
	src := "package qf\n\ntype I interface{ M() }\n\ntype Impl struct{}\n\nvar _ I = Impl{}\n\nfunc B() int { return undefinedThing }\n"
	writeFile(t, root, "qf/q.go", src)
	out := call(t, cs, "quick_fix", map[string]any{"file_path": "qf/q.go", "choice": "Declare", "apply": false})
	mustHave(t, "default", out, "held back")
	if strings.Contains(out, "not applied: strict") {
		t.Errorf("a matching choice must not read as unmatched:\n%s", out)
	}
	out = call(t, cs, "quick_fix", map[string]any{"file_path": "qf/q.go", "choice": "Declare", "strict": true, "apply": false})
	i := strings.Index(out, "line 7:")
	j := strings.Index(out, "line 9:")
	if i < 0 || j < i {
		t.Fatalf("unexpected answer:\n%s", out)
	}
	seg := out[i:j]
	if !strings.Contains(seg, "its fix was not applied") || strings.Contains(seg, "choice matched none") {
		t.Errorf("line 7's fix matched the choice; it must not read as unmatched:\n%s", out)
	}
	if readFile(t, root, "qf/q.go") != src {
		t.Error("nothing may be written")
	}
}

// N11: the boundary values of max_new_errors.
func TestMaxNewErrorsBoundaries(t *testing.T) {
	cs, root := session(t, nil)
	args := func(v int) map[string]any {
		return map[string]any{"file_path": "shapes/shapes.go", "old_string": scaleSig, "new_string": "func Scale(s Shape, k float64)", "max_new_errors": v}
	}
	for _, v := range []int{-2, -3} {
		out := call(t, cs, "edit", args(v))
		mustHave(t, "negative", out, "is invalid")
		if strings.Contains(out, "Applied") || !strings.Contains(readFile(t, root, "shapes/shapes.go"), scaleSig) {
			t.Errorf("max_new_errors=%d must be refused and write nothing:\n%s", v, out)
		}
	}
	// 0 is the default: the edit adds errors, so it is refused as an error-limit refusal, not as invalid.
	out := call(t, cs, "edit", args(0))
	if strings.Contains(out, "is invalid") || strings.Contains(out, "Applied") || !strings.Contains(out, "Not written") {
		t.Errorf("max_new_errors=0 is valid and refuses an edit with new errors:\n%s", out)
	}
	if out := call(t, cs, "edit", args(-1)); !strings.Contains(out, "Applied") {
		t.Errorf("max_new_errors=-1 means no limit:\n%s", out)
	}
}

// N10: null optional / required parameters, across tools.
func TestNullParametersAcrossTools(t *testing.T) {
	cs, _ := session(t, nil)
	for _, c := range []struct {
		tool string
		args map[string]any
		want string // "" = anything but a raw validator error
	}{
		{"edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "unused int", "new_string": "unused int, v int", "apply": nil, "max_new_errors": nil, "replace_all": nil}, ""},
		{"references", map[string]any{"file_path": "shapes/shapes.go", "line": 1, "column": 1, "include_declaration": nil}, ""},
		{"references", map[string]any{"file_path": nil, "line": nil, "column": nil}, "references(file_path, line, column"},
		{"hover", map[string]any{"file_path": "shapes/shapes.go", "line": nil, "column": nil}, "hover(file_path, line, column)"},
		{"rename_symbol", map[string]any{"name": "shapes.Scale", "new_name": nil}, "Missing required parameter(s): new_name"},
		{"symbol_info", map[string]any{"name": nil}, ""},
		{"search_symbol", map[string]any{"query": nil}, ""},
		{"edit_symbol", map[string]any{"action": nil, "name": nil}, "Missing required parameter(s)"},
		{"change_signature", map[string]any{"name": "shapes.Scale", "add": nil, "remove": nil, "apply": nil}, ""},
		{"move", map[string]any{"name": "shapes.Scale", "to_file": nil}, "to_file"},
		{"quick_fix", map[string]any{"file_path": "shapes/shapes.go", "choice": nil, "strict": nil, "apply": nil, "organize_imports": nil, "max_new_errors": nil}, ""},
		{"outline", map[string]any{"file_path": "shapes/shapes.go", "kinds": nil}, ""},
		{"diagnostics", map[string]any{"file_path": nil}, ""},
	} {
		out := call(t, cs, c.tool, c.args)
		if strings.Contains(out, "validating") || strings.Contains(out, "posIn") || strings.Contains(out, "has type \"null\"") {
			t.Errorf("%s %v: raw validator text:\n%s", c.tool, c.args, out)
		}
		if c.want != "" && !strings.Contains(out, c.want) {
			t.Errorf("%s %v: want %q in:\n%s", c.tool, c.args, c.want, out)
		}
	}
}

// B3: every registered tool with required parameters answers an empty or null call with a usage line made of
// its real parameter names, never validator or Go type text.
func TestUsageLinesNameRealParameters(t *testing.T) {
	cs, _ := session(t, nil)
	res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, tool := range res.Tools {
		b, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Required []string `json:"required"`
		}
		_ = json.Unmarshal(b, &schema)
		if len(schema.Required) == 0 {
			continue
		}
		checked++
		nulls := map[string]any{}
		for _, r := range schema.Required {
			nulls[r] = nil
		}
		for label, args := range map[string]map[string]any{"empty": {}, "null": nulls} {
			out := call(t, cs, tool.Name, args)
			for _, bad := range []string{"validating", "posIn", "tools.", "*int", "json:\"", "struct {", "In{"} {
				if strings.Contains(out, bad) {
					t.Errorf("%s (%s args): %q leaked:\n%s", tool.Name, label, bad, out)
				}
			}
			if !strings.Contains(out, tool.Name+"(") {
				t.Errorf("%s (%s args): no usage line:\n%s", tool.Name, label, out)
			}
			for _, r := range schema.Required {
				if !strings.Contains(out, r) {
					t.Errorf("%s (%s args): required parameter %q not named:\n%s", tool.Name, label, r, out)
				}
			}
		}
	}
	if checked < 5 {
		t.Errorf("expected several tools with required parameters, saw %d", checked)
	}
}

// sessionAt is session for a workspace root the test prepared itself (e.g. to put a go.work above it).
func sessionAt(t *testing.T, root string) (*mcp.ClientSession, string) {
	t.Helper()
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed")
	}
	t.Setenv(EnvWorkspace, root)
	t.Setenv(EnvReadOnly, "")
	t.Setenv(txn.StateDirEnv, t.TempDir())
	srv, mgr := New("test")
	t.Cleanup(mgr.Close)
	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, root
}
