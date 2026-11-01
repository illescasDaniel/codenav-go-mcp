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

// Item 5: the "NOT rewritten" note needs a sibling module that really refers to what the edit changes,
// resolved by types: an unrelated method or function of the same name in a sibling does not count, and a
// sibling that does use it counts whichever sibling it is.
func TestSiblingNoteIsResolvedByTypeNotName(t *testing.T) {
	const note = "NOT rewritten"
	cs, root := session(t, nil)
	noWorkFixture(t, root)
	writeFile(t, root, "outdep/o.go", "package outdep\n\nfunc Hello() string { return \"hi\" }\n\ntype zz struct{}\n\nfunc (zz) NoCaller() {}\n\nfunc Unref() int { return 1 }\n\nfunc New() zz { return zz{} }\n")
	// appmod has lookalikes of the edited names (own type with NoCaller, own Unref) and uses Hello.
	writeFile(t, root, "appmod/main.go", "package main\n\nimport \"example.com/outdep\"\n\ntype zz struct{}\n\nfunc (zz) NoCaller() {}\n\nfunc Unref() int { return 2 }\n\nfunc main() { _ = outdep.Hello(); zz{}.NoCaller(); _ = Unref() }\n")
	// libmod only has lookalikes and imports outdep without using the changed names.
	writeFile(t, root, "libmod/go.mod", "module example.com/libmod\n\ngo 1.24\n\nrequire example.com/outdep v0.0.0\n\nreplace example.com/outdep => ../outdep\n")
	writeFile(t, root, "libmod/l.go", "package libmod\n\nimport \"example.com/outdep\"\n\ntype zz struct{}\n\nfunc (zz) NoCaller() {}\n\nfunc Unref() int { return 3 }\n\nvar _ = outdep.Hello\n\nfunc L() { zz{}.NoCaller(); _ = Unref() }\n")
	for name, c := range map[string]struct {
		tool string
		args map[string]any
		note bool
	}{
		"rename lookalike method":   {"edit", map[string]any{"file_path": "outdep/o.go", "old_string": "func (zz) NoCaller() {}", "new_string": "func (zz) NoCaller2() {}", "apply": false}, false},
		"change lookalike method":   {"edit", map[string]any{"file_path": "outdep/o.go", "old_string": "func (zz) NoCaller() {}", "new_string": "func (zz) NoCaller(n int) {}", "apply": false}, false},
		"change lookalike function": {"edit", map[string]any{"file_path": "outdep/o.go", "old_string": "func Unref() int", "new_string": "func Unref(n int) int", "apply": false}, false},
		"rename lookalike function": {"rename_symbol", map[string]any{"name": "outdep.Unref", "new_name": "Unref2", "apply": false}, false},
		"change used function":      {"edit", map[string]any{"file_path": "outdep/o.go", "old_string": "func Hello() string", "new_string": "func Hello() (string, error)", "apply": false}, true},
	} {
		out := call(t, cs, c.tool, c.args)
		if has := strings.Contains(out, note); has != c.note {
			t.Errorf("%s: note shown = %v, want %v:\n%s", name, has, c.note, out)
		}
	}
	// The sibling that really uses the symbol decides, in whatever order the siblings are checked.
	writeFile(t, root, "appmod/main.go", "package main\n\nfunc main() {}\n")
	out := call(t, cs, "edit", map[string]any{"file_path": "outdep/o.go", "old_string": "func Hello() string", "new_string": "func Hello() (string, error)", "apply": false})
	if !strings.Contains(out, note) {
		t.Errorf("libmod uses Hello (var _ = outdep.Hello):\n%s", out)
	}
	writeFile(t, root, "libmod/l.go", "package libmod\n\nimport \"example.com/outdep\"\n\nvar _ = outdep.New\n")
	out = call(t, cs, "edit", map[string]any{"file_path": "outdep/o.go", "old_string": "func Hello() string", "new_string": "func Hello() (string, error)", "apply": false})
	if strings.Contains(out, note) {
		t.Errorf("no sibling uses Hello now:\n%s", out)
	}
}

// Item 6: the leftover scan of rename_symbol lists only what the type checker did not link: identifiers
// that resolve to a different object (func-type parameters, other types' fields and methods, methods of
// other packages) are not leftovers.
func TestRenameLeftoverScanIgnoresOtherObjects(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "lo/lo.go", `package lo

import "sync"

func name(x int) int { return x }

type Handler struct {
	cb func(name string)
}

func Use() int { return name(1) }

type Conf struct {
	name string
	mu   sync.Mutex
}

func (c *Conf) Lock()   { c.mu.Lock() }
func (c *Conf) Unlock() { c.mu.Unlock() }

type Other struct{ name string }

type Iface interface{ name() string }

type Impl struct{ name string }

func (Impl) Name() string { return "" }

func Read(c *Conf, o Other, i Impl) (string, string, string) { return c.name, o.name, i.name }
`)
	writeFile(t, root, "lo/other.go", "package lo\n\nimport \"sync\"\n\ntype Box struct{ mu sync.Mutex }\n\nfunc (b *Box) Do(c *Conf) { b.mu.Lock(); defer b.mu.Unlock(); c.Lock(); c.mu.Lock() }\n")
	writeFile(t, root, "lo2/use.go", "package lo2\n\nimport (\n\t\"sync\"\n\n\t\"example.com/fx/lo\"\n)\n\ntype Wrap struct{ mu sync.Mutex }\n\nfunc (w *Wrap) Do(c *lo.Conf) { c.Lock(); w.mu.Lock(); w.mu.Unlock(); c.Unlock() }\n")
	for label, c := range map[string]map[string]any{
		"package func": {"name": "lo.name", "new_name": "label"},
		"Conf.name":    {"name": "lo.Conf.name", "new_name": "label"},
		"Other.name":   {"name": "lo.Other.name", "new_name": "label"},
		"Impl.name":    {"name": "lo.Impl.name", "new_name": "label"},
		"Conf.Lock":    {"name": "lo.Conf.Lock", "new_name": "Acquire"},
		"Conf.Unlock":  {"name": "lo.Conf.Unlock", "new_name": "Release"},
	} {
		c["apply"] = false
		out := call(t, cs, "rename_symbol", c)
		if strings.Contains(out, "did not link") || strings.Contains(out, "still appears") {
			t.Errorf("%s: false leftover:\n%s", label, out)
		}
	}
}

// Item 12: gopls' vet-style analyzers (printf) are part of `diagnostics` and of the edit comparison while the
// package type-checks; in a package with errors gopls runs none, and the edit output says how many it hid.
func TestPrintfWarningIsReported(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "pf/pf.go", "package pf\n\nimport \"fmt\"\n\nfunc P() { fmt.Println(1) }\n")
	out := call(t, cs, "edit", map[string]any{"file_path": "pf/pf.go", "old_string": "fmt.Println(1)", "new_string": "fmt.Printf(\"%d\", \"s\")", "apply": false})
	mustHave(t, "edit", out, "1 new warning(s)", "fmt.Printf format %d has arg")
	writeFile(t, root, "pf/pf.go", "package pf\n\nimport \"fmt\"\n\nfunc P() { fmt.Printf(\"%d\", \"s\") }\n")
	out = call(t, cs, "diagnostics", map[string]any{"file_path": "pf/pf.go"})
	mustHave(t, "diagnostics", out, "fmt.Printf format %d has arg")
	out = call(t, cs, "edit", map[string]any{"file_path": "pf/pf.go", "old_string": "func P()", "new_string": "func Q() { var x int }\n\nfunc P()", "apply": false})
	mustHave(t, "package with errors", out, "1 new error(s)")
}
