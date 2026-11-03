package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callRawErr is callRaw that also reports whether the answer is a tool error (isError).
func callRawErr(t *testing.T, cs *mcp.ClientSession, name, args string) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

// ESYM-005: a new file's package clause is chosen from a sibling file, else from the directory name, and the
// answer says which.
func TestInsertIntoNewFileNamesPackageSource(t *testing.T) {
	cs, root := session(t, nil)
	out := call(t, cs, "edit_symbol", map[string]any{"action": "insert", "file_path": "c20new/n.go", "source": "func N() {}"})
	mustHave(t, "no sibling", out, "package c20new chosen from the directory name (no sibling .go files)")
	if got := readFile(t, root, "c20new/n.go"); !strings.HasPrefix(got, "package c20new\n") {
		t.Fatalf("the new file must start with the package clause:\n%s", got)
	}

	writeFile(t, root, "c20sib/sib.go", "package sibpkg\n\nfunc Sib() {}\n")
	out = call(t, cs, "edit_symbol", map[string]any{"action": "insert", "file_path": "c20sib/new.go", "source": "func N() {}"})
	mustHave(t, "sibling", out, "package sibpkg chosen from sibling file sib.go")
	if got := readFile(t, root, "c20sib/new.go"); !strings.HasPrefix(got, "package sibpkg\n") {
		t.Fatalf("the new file must take the sibling's package:\n%s", got)
	}
}

// ESYM-006: a comment-only insert is refused (it would be a no-op write called an insert); a comment that
// comes with a declaration still inserts.
func TestInsertCommentOnlyIsRefused(t *testing.T) {
	cs, root := session(t, nil)
	const file = "c20cm/p.go"
	const src = "package c20cm\n\nfunc Keep() int { return 1 }\n"
	writeFile(t, root, file, src)
	out := call(t, cs, "edit_symbol", map[string]any{"action": "insert", "file_path": file, "source": "// just a comment"})
	mustHave(t, "comment-only", out, "source holds only comments; give a declaration.")
	if got := readFile(t, root, file); got != src {
		t.Fatalf("a refused insert must not write:\n%s", got)
	}

	out = call(t, cs, "edit_symbol", map[string]any{"action": "insert", "file_path": file, "source": "// Two is two.\nfunc Two() int { return 2 }"})
	mustHave(t, "comment with a declaration", out, "Applied")
	if got := readFile(t, root, file); !strings.Contains(got, "func Two() int") {
		t.Fatalf("the declaration must be inserted:\n%s", got)
	}
}

// ESYM-007: a missing action names the values action takes, as the unknown-action refusal does.
func TestEditSymbolMissingActionListsValues(t *testing.T) {
	cs, root := session(t, nil)
	const src = "package c20act\n\nfunc F() int { return 1 }\n"
	writeFile(t, root, "c20act/p.go", src)
	out, isErr := callRawErr(t, cs, "edit_symbol", `{"name":"F","file_path":"c20act/p.go","source":"func F() int { return 2 }"}`)
	mustHave(t, "missing action", out, "Missing required parameter(s): action.", "action is one of: replace, insert or delete.")
	if !isErr {
		t.Errorf("a missing action is a tool error:\n%s", out)
	}
	if got := readFile(t, root, "c20act/p.go"); got != src {
		t.Fatal("a refused call must not write")
	}
}

// PROTO-006: arguments that are not a JSON object get a plain message with isError, not the Go decoder's text.
func TestNonObjectArgumentsAnsweredPlainly(t *testing.T) {
	cs, _ := session(t, nil)
	for _, args := range []string{`[]`, `"x"`} {
		out, isErr := callRawErr(t, cs, "workspace", args)
		mustHave(t, args, out, "arguments must be a JSON object.")
		if !isErr {
			t.Errorf("%s: want isError true, got:\n%s", args, out)
		}
		for _, leak := range []string{"unmarshal", "Go value", "map[string]"} {
			if strings.Contains(out, leak) {
				t.Errorf("%s: the Go type text %q leaked into %q", args, leak, out)
			}
		}
	}
}

// SIG-001: a method of an interface in another package changes because a type satisfies both interfaces; the
// note says which type, so the change in the other package is not unexplained.
func TestChangeSignatureNotesWhySecondPackageChanges(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "c20sig/a/store.go", "package a\n\ntype Store interface{ Load(k string) string }\n\ntype Mem struct{}\n\nfunc (Mem) Load(k string) string { return k }\n\nvar _ Store = Mem{}\n\nfunc UseA(s Store) string { return s.Load(\"x\") }\n")
	writeFile(t, root, "c20sig/b/store.go", "package b\n\ntype Store interface{ Load(k string) string }\n\ntype Mem struct{}\n\nfunc (Mem) Load(k string) string { return k }\n\nvar _ Store = Mem{}\n\nfunc UseB(s Store) string { return s.Load(\"y\") }\n")
	out := call(t, cs, "change_signature", map[string]any{"name": "Store.Load", "file_path": "c20sig/a/store.go",
		"add": []map[string]any{{"name": "opts", "type": "int", "value": "0"}}})
	mustHave(t, "reason", out, "b.Store.Load changed too because a.Mem implements both a.Store and b.Store.")
}

// SIG-005 (guard): a //go:linkname directive naming the changed func holds the write and lists the directive, both
// the push in its own file and the pull from another package. This already holds on the base commit (buildGuard);
// the test pins it.
func TestChangeSignatureHoldsLinknameDirectives(t *testing.T) {
	cs, root := session(t, nil)
	const link = "package p023\n\nimport _ \"unsafe\"\n\n//go:linkname hidden\nfunc hidden(x int) int { return x }\n\nfunc Use() int { return hidden(1) }\n"
	writeFile(t, root, "c20lnk/p023/link.go", link)
	writeFile(t, root, "c20lnk/p023b/ref.go", "package p023b\n\nimport _ \"unsafe\"\n\n//go:linkname hiddenRef example.com/fx/c20lnk/p023.hidden\nfunc hiddenRef(x int) int\n\nfunc Ref() int { return hiddenRef(1) }\n")
	out := call(t, cs, "change_signature", map[string]any{"name": "hidden", "file_path": "c20lnk/p023/link.go",
		"add": []map[string]any{{"name": "c", "type": "int", "value": "0"}}})
	mustHave(t, "linkname hold", out, "Not written", "hidden is named by //go:linkname", "c20lnk/p023/link.go:5 (//go:linkname hidden)", "c20lnk/p023b/ref.go:5")
	if got := readFile(t, root, "c20lnk/p023/link.go"); got != link {
		t.Fatalf("a held change must not write:\n%s", got)
	}
}
