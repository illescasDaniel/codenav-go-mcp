package server

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func batchCall(t *testing.T, cs *mcp.ClientSession, calls []any, stop any) string {
	t.Helper()
	args := map[string]any{"calls": calls}
	if stop != nil {
		args["stop_on_error"] = stop
	}
	return call(t, cs, "batch", args)
}

func TestBatchReadsAndValidationOverMCP(t *testing.T) {
	cs, _ := session(t, nil)
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range list.Tools {
		if tool.Name == "batch" {
			found = true
		}
	}
	if !found {
		t.Fatal("batch missing from tools/list")
	}

	out := batchCall(t, cs, []any{
		map[string]any{"tool": "outline", "args": map[string]any{"file_path": "shapes/shapes.go"}},
		map[string]any{"tool": "not_a_tool"},
		map[string]any{"name": "symbol_info", "name_unused": "unused", "args": map[string]any{"name": "shapes.Scale"}},
		map[string]any{"tool": "symbol_info", "name": "shapes.Scale"},
	}, nil)
	if !strings.Contains(out, "[1/4]") || !strings.Contains(out, "[2/4]") || !strings.Contains(out, "[4/4]") || !strings.Contains(out, "Scale") || !strings.Contains(out, "2/4 ok, 2 failed") {
		t.Fatalf("read failures should be isolated and later calls run:\n%s", out)
	}
	if strings.Index(out, "[1/4]") > strings.Index(out, "[4/4]") {
		t.Fatalf("out of order:\n%s", out)
	}
	out = batchCall(t, cs, []any{map[string]any{"tool": "batch", "args": map[string]any{"calls": []any{}}}}, nil)
	if !strings.Contains(out, "nested") {
		t.Fatalf("nested batch must be refused: %s", out)
	}
	out = batchCall(t, cs, []any{}, nil)
	if !strings.Contains(out, "non-empty") {
		t.Fatalf("empty batch must be refused: %s", out)
	}
	many := make([]any, 26)
	for i := range many {
		many[i] = map[string]any{"tool": "workspace"}
	}
	out = batchCall(t, cs, many, nil)
	if !strings.Contains(out, "25") {
		t.Fatalf("oversize batch must be refused: %s", out)
	}
	out = batchCall(t, cs, []any{
		map[string]any{"tool": "outline", "args": map[string]any{"file_path": 23}},
		map[string]any{"tool": "outline", "args": map[string]any{"file_path": "shapes/shapes.go"}},
	}, true)
	if !strings.Contains(out, "1 failed") || !strings.Contains(out, "1 skipped") || strings.Contains(out, "[2/2]") {
		t.Fatalf("invalid typed argument must stop a read batch when requested:\n%s", out)
	}
}

func TestBatchWritesSequentialStopAndContinue(t *testing.T) {
	cs, root := session(t, nil)
	edit := func(old, new string) map[string]any {
		return map[string]any{"tool": "edit", "args": map[string]any{"file_path": "shapes/shapes.go", "old_string": old, "new_string": new}}
	}
	out := batchCall(t, cs, []any{edit("// Shape is a port.", "// Shape is a port!"), edit("// Shape is a port!", "// Shape is a port?"), map[string]any{"tool": "outline", "args": map[string]any{"file_path": "shapes/shapes.go"}}}, nil)
	if !strings.Contains(out, "3/3 ok") || !strings.Contains(readFile(t, root, "shapes/shapes.go"), "// Shape is a port?") {
		t.Fatalf("dependent edits must apply in order:\n%s", out)
	}
	out = batchCall(t, cs, []any{edit("not present", "never"), edit("// Shape is a port?", "// should not run")}, nil)
	if !strings.Contains(out, "1 failed") || !strings.Contains(out, "1 skipped") || strings.Contains(readFile(t, root, "shapes/shapes.go"), "should not run") {
		t.Fatalf("failure must stop write batch:\n%s", out)
	}
	out = batchCall(t, cs, []any{edit("not present", "never"), edit("// Shape is a port?", "// continued")}, false)
	if !strings.Contains(out, "1/2 ok") || !strings.Contains(readFile(t, root, "shapes/shapes.go"), "// continued") {
		t.Fatalf("explicit false must continue:\n%s", out)
	}
}

func TestBatchReadOnlyAndPreview(t *testing.T) {
	cs, root := session(t, map[string]string{EnvReadOnly: "1"})
	before := readFile(t, root, "shapes/shapes.go")
	edit := func(apply bool) map[string]any {
		return map[string]any{"tool": "edit", "args": map[string]any{"file_path": "shapes/shapes.go", "old_string": "// Shape is a port.", "new_string": "// Shape is a port!", "apply": apply}}
	}
	out := batchCall(t, cs, []any{edit(false), map[string]any{"tool": "outline", "args": map[string]any{"file_path": "shapes/shapes.go"}}}, nil)
	if !strings.Contains(out, "Preview only") || !strings.Contains(out, "2/2 ok") {
		t.Fatalf("preview should succeed:\n%s", out)
	}
	out = batchCall(t, cs, []any{edit(true), map[string]any{"tool": "workspace"}}, nil)
	if !strings.Contains(out, "1 failed") || !strings.Contains(out, "1 skipped") {
		t.Fatalf("read-only write should fail and stop:\n%s", out)
	}
	if readFile(t, root, "shapes/shapes.go") != before {
		t.Fatal("read-only batch wrote a file")
	}
}
