package server

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// BUG-SRCH-009 (block 1 of the fix plan): isError is true when a call was refused or failed, and false for a
// valid query with no match and for an answer or a preview that did not refuse anything.

// callIsError returns the answer text and the isError flag of one tool call.
func callIsError(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
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

func TestIsErrorContract(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "shapes/stub.go", "package shapes\n\ntype Stub struct{}\n\nvar _ Shape = Stub{}\n")
	before := readFile(t, root, "shapes/shapes.go")
	for _, c := range []struct {
		name    string
		tool    string
		args    map[string]any
		wantErr bool
		want    string
	}{
		{"refused input: unknown kind", "search_symbol", map[string]any{"query": "Scale", "kind": "bogus"}, true, "unknown symbol kind"},
		{"refused input: missing argument", "symbol_info", map[string]any{}, true, "Pass `name`"},
		{"refused input: file outside the workspace", "hover", map[string]any{"file_path": "../nope/x.go", "line": 1, "column": 1}, true, "outside the workspace"},
		{"refused write: file outside the workspace", "edit", map[string]any{"file_path": "../nope/x.go", "new_text": "package nope\n", "apply": false}, true, "outside the workspace"},
		{"refused write: new errors over max_new_errors", "edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "func Scale(", "new_string": "func Scale2("}, true, "Not written"},
		{"refused write: name matches no symbol", "rename_symbol", map[string]any{"name": "shapes.Nope", "new_name": "Other", "apply": false}, true, "No symbol found matching"},
		{"refused: nothing to undo", "undo_edit", map[string]any{}, true, "nothing to undo"},
		{"refused write: quick_fix choice matches no fix", "quick_fix", map[string]any{"file_path": "shapes/stub.go", "choice": "zzz"}, true, "No fix matches choice"},
		{"valid query with no match", "search_symbol", map[string]any{"query": "ZzzNoSuchName"}, false, "No symbols matching"},
		{"read: name matches no symbol", "symbol_info", map[string]any{"name": "shapes.Nope"}, false, "No symbol found matching"},
		{"read: successful", "symbol_info", map[string]any{"name": "shapes.Scale"}, false, "Scale"},
		{"write preview", "edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "unused int", "new_string": "unused int64", "apply": false}, false, "Preview only"},
	} {
		out, isErr := callIsError(t, cs, c.tool, c.args)
		if isErr != c.wantErr {
			t.Errorf("%s: isError=%v, want %v:\n%s", c.name, isErr, c.wantErr, out)
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("%s: answer lacks %q:\n%s", c.name, c.want, out)
		}
	}
	if readFile(t, root, "shapes/shapes.go") != before {
		t.Error("the refused and preview calls must not write")
	}
}

func TestIsErrorReadOnlyRefusal(t *testing.T) {
	cs, _ := session(t, map[string]string{EnvReadOnly: "1"})
	out, isErr := callIsError(t, cs, "edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "unused int", "new_string": "unused int64"})
	if !isErr || !strings.Contains(out, "Not written") {
		t.Errorf("read-only write must be an error (isError=%v):\n%s", isErr, out)
	}
	out, isErr = callIsError(t, cs, "edit", map[string]any{"file_path": "shapes/shapes.go", "old_string": "unused int", "new_string": "unused int64", "apply": false})
	if isErr || !strings.Contains(out, "Preview only") {
		t.Errorf("read-only preview is not a refusal (isError=%v):\n%s", isErr, out)
	}
}
