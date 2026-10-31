package tools

import (
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
