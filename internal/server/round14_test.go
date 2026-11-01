package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Regression tests for the fixes of round 14 (one test per fix).

// callRaw sends the arguments as written, so a number keeps its text (1e400 does not fit a Go float).
func callRaw(t *testing.T, cs *mcp.ClientSession, name, args string) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// Items 1 + 11: every integer parameter beyond 2^53 (1e400, 2^63, 2^53+1, 1e23) gets the same sentence,
// echoing the number as the caller wrote it.
func TestHugeNumbersAnswerAlikeAndEchoOriginal(t *testing.T) {
	cs, _ := session(t, nil)
	for _, n := range []string{"1e400", "-1e400", "9223372036854775808", "9007199254740993", "1e23", "99999999999999999999999"} {
		out := callRaw(t, cs, "hover", `{"file_path":"shapes/shapes.go","line":3,"column":`+n+`}`)
		mustHave(t, n, out, "column must be a whole number of reasonable size (got "+n+", which does not fit).", "Usage: hover(")
		for _, bad := range []string{"unmarshal", "validating", "json:", "9007199254740992", "e+23"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: %q leaked:\n%s", n, bad, out)
			}
		}
	}
	// Small exponent forms stay valid.
	if out := callRaw(t, cs, "hover", `{"file_path":"shapes/shapes.go","line":3,"column":1e1}`); strings.Contains(out, "reasonable size") {
		t.Errorf("1e1 is a fine column:\n%s", out)
	}
}

// Item 2: a go.work member five levels up is shown as "../../../../../lib/lib.go" by every tool (it used to
// be absolute past three levels, and absolute in rename_symbol's refusal at every depth).
func TestFarAwayModulePathsAreRelativeEverywhere(t *testing.T) {
	top := t.TempDir()
	writeFile(t, top, "go.work", "go 1.24\n\nuse (\n\t./a/b/c/d/app\n\t./lib\n)\n")
	writeFile(t, top, "lib/go.mod", "module example.com/lib\n\ngo 1.24\n\nrequire example.com/app v0.0.0\n")
	writeFile(t, top, "lib/lib.go", "package lib\n\nimport \"example.com/app\"\n\ntype Doer interface{ Do() }\n\nfunc Hello() string { return \"hi\" }\n\nfunc UseApp() { app.Foo(); var _ Doer = app.T{} }\n")
	writeFile(t, top, "a/b/c/d/app/go.mod", "module example.com/app\n\ngo 1.24\n\nrequire example.com/lib v0.0.0\n")
	writeFile(t, top, "a/b/c/d/app/app.go", "package app\n\nimport \"example.com/lib\"\n\nfunc Foo() {}\n\ntype T struct{}\n\nfunc (T) Do() {}\n\nfunc Main() { _ = lib.Hello() }\n")
	cs, _ := sessionAt(t, top+"/a/b/c/d/app")
	const far = "../../../../../lib/lib.go"
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"callers", map[string]any{"name": "app.Foo"}},
		{"implementations", map[string]any{"name": "app.T.Do"}},
		{"symbol_info", map[string]any{"name": "app.Foo"}},
		{"symbol_info", map[string]any{"name": "lib.Hello"}},
		{"rename_symbol", map[string]any{"name": "lib.Hello", "new_name": "Hi", "apply": false}},
	} {
		out := call(t, cs, c.tool, c.args)
		mustHave(t, c.tool, out, far)
		if strings.Contains(out, top) {
			t.Errorf("%s %v: absolute path leaked:\n%s", c.tool, c.args, out)
		}
	}
}

// Items 3 + 8: "outside the workspace" never prints the absolute workspace root, and a file_path the caller
// pointed edit at is refused as such (not as "a reference or declaration").
func TestOutsideWorkspaceMessagesDoNotPrintTheRoot(t *testing.T) {
	cs, root := session(t, nil)
	for _, c := range []struct {
		tool string
		args map[string]any
		want []string
	}{
		{"outline", map[string]any{"file_path": "../x.go"}, []string{"../x.go is outside the workspace"}},
		{"outline", map[string]any{"file_path": "/etc/passwd"}, []string{"/etc/passwd is outside the workspace"}},
		{"diagnostics", map[string]any{"file_path": "/etc/passwd"}, []string{"/etc/passwd is outside the workspace"}},
		{"edit", map[string]any{"file_path": "/etc/hosts", "new_text": "package x\n"}, []string{"/etc/hosts is outside the workspace; the write tools only change files inside it"}},
		{"edit", map[string]any{"file_path": "../x.go", "new_text": "package x\n"}, []string{"../x.go is outside the workspace; the write tools only change files inside it"}},
	} {
		out := call(t, cs, c.tool, c.args)
		mustHave(t, c.tool, out, c.want...)
		if strings.Contains(out, root) || strings.Contains(out, "a reference or declaration") {
			t.Errorf("%s %v:\n%s", c.tool, c.args, out)
		}
	}
}

// Item 4: a workspace without a go.mod has packages without an import path; hover and symbol_info print no
// pkg.go.dev link (gopls makes up `pkg.go.dev/_/<absolute dir>#F`).
func TestNoGoModHoverHasNoBogusLink(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "nomod/f.go", "package nomod\n\n// F does a thing.\nfunc F() {}\n\nfunc G() { F() }\n")
	cs, _ := sessionAt(t, root)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"hover", map[string]any{"file_path": "nomod/f.go", "line": 4, "column": 6}},
		{"symbol_info", map[string]any{"name": "nomod.F"}},
	} {
		out := call(t, cs, c.tool, c.args)
		mustHave(t, c.tool, out, "F does a thing")
		if strings.Contains(out, "pkg.go.dev") || strings.Contains(out, root) {
			t.Errorf("%s: bogus link or absolute path:\n%s", c.tool, out)
		}
	}
}

// Item 9: a NUL byte in a path is refused as such (it used to print as a space in "Cannot read a b.go"), and an
// empty required file_path is reported as empty (quick_fix used to echo ".").
func TestNulAndEmptyPathArguments(t *testing.T) {
	cs, _ := session(t, nil)
	for _, tool := range []string{"outline", "hover", "quick_fix", "edit", "diagnostics"} {
		out := callRaw(t, cs, tool, `{"file_path":"a\u0000b.go","line":1,"column":1,"old_string":"x","new_string":"y"}`)
		mustHave(t, tool+" NUL", out, "file_path contains a NUL byte")
		if strings.Contains(out, "Cannot read") {
			t.Errorf("%s: raw read error:\n%s", tool, out)
		}
	}
	out := callRaw(t, cs, "move", `{"name":"shapes.Scale","to_file":"x\u0000.go"}`)
	mustHave(t, "move NUL", out, "to_file contains a NUL byte")
	for _, tool := range []string{"quick_fix", "outline", "hover"} {
		out := callRaw(t, cs, tool, `{"file_path":"","line":1,"column":1}`)
		mustHave(t, tool+" empty", out, "file_path is empty")
		if strings.Contains(out, `"."`) {
			t.Errorf("%s: echoes \".\":\n%s", tool, out)
		}
	}
	// Optional file_path: "" still means "not given".
	if out := callRaw(t, cs, "rename_symbol", `{"name":"shapes.Scale","new_name":"S2","file_path":"","apply":false}`); strings.Contains(out, "file_path is empty") {
		t.Errorf("an optional file_path may be empty:\n%s", out)
	}
}

// Item 7: hover on a cgo call site shows C.int, not the markdown-escaped \_Ctype\_int.
func TestCgoHoverShowsCTypeNames(t *testing.T) {
	skipWithoutCgo(t)
	cs, root := session(t, nil)
	writeFile(t, root, "exp/exp.go", "package exp\n\n/*\n#include <stdint.h>\nextern int goCallback(int);\nstatic int callit(int x) { return goCallback(x); }\n*/\nimport \"C\"\n\n//export goCallback\nfunc goCallback(x C.int) C.int { return x * 2 }\n\nfunc Run() int { return int(C.callit(3)) }\n")
	for col := 20; col <= 42; col++ {
		out := call(t, cs, "hover", map[string]any{"file_path": "exp/exp.go", "line": 13, "column": col})
		if strings.Contains(out, "_Ctype") || strings.Contains(out, "_Cfunc") {
			t.Errorf("column %d shows cgo's generated names:\n%s", col, out)
		}
	}
	out := call(t, cs, "hover", map[string]any{"file_path": "exp/exp.go", "line": 11, "column": 20})
	mustHave(t, "param type", out, "C.int")
}
