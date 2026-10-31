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
