package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShortQueryAndArticle(t *testing.T) {
	if got := shortQuery(strings.Repeat("a", 200)); len([]rune(got)) != 83 || !strings.HasSuffix(got, "...") {
		t.Errorf("shortQuery: %q", got)
	}
	if shortQuery("abc") != "abc" {
		t.Error("short queries stay as they are")
	}
	if withArticle("interface") != "an interface" || withArticle("struct") != "a struct" {
		t.Errorf("withArticle: %q %q", withArticle("interface"), withArticle("struct"))
	}
}

func TestFriendlyTypeError(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`validating /properties/line: type: 5 has type "string", want integer`, `line must be an integer (got string "5").`},
		{`validating /properties/line: type: 1.5 has type "number", want integer`, `line must be an integer (got number 1.5).`},
		{`validating /properties/apply: type: yes has type "string", want boolean`, `apply must be a boolean (true or false) (got string "yes").`},
		{`validating /properties/new_name: type: 5 has type "integer", want string`, `new_name must be a string (got integer 5).`},
		{`validating /properties/max_new_errors: type: x has type "string", want [integer null]`, `max_new_errors must be an integer (got string "x").`},
		{`validating /properties/add: type: map[name:x] has type "object", want array`, `add must be an array (got object).`},
		{`validating /properties/add/items: type: x has type "string", want object`, `each item of add must be an object (got string "x").`},
		{`validating /properties/add/items/0: unexpected additional properties ["default"]`, `unknown field default in an item of add. Go has no default values: use "value", the argument written at each existing call.`},
		{`validating /properties/add/items/0: unexpected additional properties ["foo"]`, `unknown field foo in an item of add.`},
		{`unexpected additional properties ["bogus"]`, `unknown parameter(s): bogus.`},
		{`validating /properties/line: type: 5 has type "string", want null`, ``}, // nothing sensible to say
		{`required: missing properties: ["name"]`, ``},
		{`something else entirely`, ``},
	} {
		if got := friendlyTypeError(c.in); got != c.want {
			t.Errorf("friendlyTypeError(%q)\n got  %q\n want %q", c.in, got, c.want)
		}
	}
}

func TestCheckWrappedCountsLinesFromTheSnippet(t *testing.T) {
	// header lines of the wrapper are not counted: the error is on line 3 of the snippet.
	err := checkWrapped("package p\nfunc A() {}\n\nfunc B( {", 1)
	if err == nil || !strings.Contains(err.Error(), "source line 3") || strings.Contains(err.Error(), "source.go") {
		t.Errorf("one header line: %v", err)
	}
	err = checkWrapped("package p\ntype T struct {\nH int\nV int int int\n}", 2)
	if err == nil || !strings.Contains(err.Error(), "source line 2") {
		t.Errorf("two header lines: %v", err)
	}
	if err := checkWrapped("package p\nfunc A() {}", 1); err != nil {
		t.Errorf("valid snippet: %v", err)
	}
}

func TestCapOutput(t *testing.T) {
	if got := capOutput("short"); got != "short" {
		t.Errorf("under the cap nothing changes: %q", got)
	}
	exact := strings.Repeat("a", maxOutputBytes)
	if got := capOutput(exact); got != exact {
		t.Error("exactly the cap is allowed")
	}
	// Lines of 99 bytes + newline: cut at the last newline before the cap, and count what was left out.
	line := strings.Repeat("b", 99) + "\n"
	in := strings.Repeat(line, maxOutputBytes/100+500)
	got := capOutput(in)
	cut := strings.LastIndex(in[:maxOutputBytes], "\n")
	want := fmt.Sprintf("%s\n... %d more byte(s) omitted (the answer is over %d bytes; narrow the request).", in[:cut], len(in)-cut, maxOutputBytes)
	if got != want {
		t.Errorf("cap at a line boundary: got tail %q", got[max(0, len(got)-120):])
	}
	// One huge line without a newline in the second half: cut at the cap itself.
	one := strings.Repeat("c", maxOutputBytes+1234)
	got = capOutput(one)
	if !strings.HasPrefix(got, one[:maxOutputBytes]) || !strings.Contains(got, "1234 more byte(s) omitted") {
		t.Errorf("single long line: tail %q", got[max(0, len(got)-120):])
	}
}

// N8: scrub is the one place absolute workspace paths become relative.
func TestScrubWorkspacePaths(t *testing.T) {
	w := &Workspace{Root: "/ws/proj"}
	for _, c := range []struct{ in, want string }{
		{"method T.M already declared at /ws/proj/a/b.go:3:4", "method T.M already declared at a/b.go:3:4"},
		{"/ws/proj/x.go:1:1: a; /ws/proj/y.go:2:2: b", "x.go:1:1: a; y.go:2:2: b"},
		{"import cycle: /ws/proj/p/p.go imports /ws/proj/q/q.go", "import cycle: p/p.go imports q/q.go"},
		{"other /ws/proj2/x.go", "other /ws/proj2/x.go"},     // another directory sharing the prefix
		{"in /other/ws/proj/x.go", "in /other/ws/proj/x.go"}, // not at a word boundary
		{"no paths here", "no paths here"},
	} {
		if got := w.scrub(c.in); got != c.want {
			t.Errorf("scrub(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := (&Workspace{}).scrub("/ws/proj/x.go"); got != "/ws/proj/x.go" {
		t.Errorf("no root, nothing to do: %q", got)
	}
	// A symlinked root: messages may carry the resolved path.
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	ws := &Workspace{Root: link}
	if got := ws.scrub("at " + real + "/a.go:1:1 and " + link + "/b.go:2:2"); got != "at a.go:1:1 and b.go:2:2" {
		t.Errorf("both spellings of the root: %q", got)
	}
}
