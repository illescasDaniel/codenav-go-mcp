package format

import (
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// BUG-LEAK-003 / SHAPE-008: a "no required module" BrokenImport gets the cause of the go command in its parenthetical.
func TestBlock21F3_ExplainBrokenImportsReplacesParenthetical(t *testing.T) {
	const generic = `could not import example.com/dep (no required module provides package "example.com/dep")`
	items := []lsp.Diagnostic{
		{Message: generic, Severity: 1},
		{Message: `undefined: foo`, Severity: 1},
		{Message: `could not import example.com/other (no required module provides package "example.com/other")`, Severity: 1},
	}
	var asked []string
	got := ExplainBrokenImports(items, func(p string) string {
		asked = append(asked, p)
		if p == "example.com/dep" {
			return "parsing dep/go.mod: dep/go.mod:7: syntax error"
		}
		return ""
	})
	if len(got) != len(items) {
		t.Fatalf("len = %d, want %d", len(got), len(items))
	}
	want := "could not import example.com/dep (parsing dep/go.mod: dep/go.mod:7: syntax error)"
	if got[0].Message != want {
		t.Errorf("message = %q, want %q", got[0].Message, want)
	}
	if got[1].Message != "undefined: foo" {
		t.Errorf("unrelated message changed: %q", got[1].Message)
	}
	if got[2].Message != items[2].Message {
		t.Errorf("unknown cause must keep the message, got %q", got[2].Message)
	}
	if items[0].Message != generic {
		t.Errorf("input was modified: %q", items[0].Message)
	}
	if strings.Join(asked, ",") != "example.com/dep,example.com/other" {
		t.Errorf("cause asked for %v", asked)
	}
}

// A message that names two different paths, or has no quoted package, is not a BrokenImport of that shape.
func TestBlock21F3_ExplainBrokenImportsIgnoresOtherShapes(t *testing.T) {
	items := []lsp.Diagnostic{
		{Message: `could not import a (no required module provides package "b")`},
		{Message: `could not import a (missing go.sum entry)`},
	}
	got := ExplainBrokenImports(items, func(string) string {
		t.Fatal("cause must not be asked for a message of another shape")
		return ""
	})
	for i := range items {
		if got[i].Message != items[i].Message {
			t.Errorf("message %d changed: %q", i, got[i].Message)
		}
	}
}

// At most maxBrokenImportCauses distinct import paths are looked up in one answer.
func TestBlock21F3_ExplainBrokenImportsLookupLimit(t *testing.T) {
	var items []lsp.Diagnostic
	for _, p := range []string{"a.com/1", "a.com/2", "a.com/3", "a.com/4", "a.com/5", "a.com/6", "a.com/1"} {
		items = append(items, lsp.Diagnostic{Message: `could not import ` + p + ` (no required module provides package "` + p + `")`})
	}
	calls := 0
	got := ExplainBrokenImports(items, func(p string) string {
		calls++
		return "cause of " + p
	})
	if calls != maxBrokenImportCauses {
		t.Errorf("cause called %d times, want %d", calls, maxBrokenImportCauses)
	}
	if !strings.HasPrefix(got[4].Message, "could not import a.com/5 (cause of a.com/5)") {
		t.Errorf("fifth path not explained: %q", got[4].Message)
	}
	if got[5].Message != items[5].Message {
		t.Errorf("sixth path must keep its message: %q", got[5].Message)
	}
	if !strings.HasPrefix(got[6].Message, "could not import a.com/1 (cause of a.com/1)") {
		t.Errorf("repeat of a looked-up path: %q", got[6].Message)
	}
}
