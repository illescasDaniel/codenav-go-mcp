package format

import (
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

func sym(name string) *lsp.Symbol {
	return &lsp.Symbol{Name: name, Kind: 12, Location: &lsp.Location{URI: "file:///r/" + name + ".go"}}
}

func TestFuzzyHidesWeakMatches(t *testing.T) {
	syms := []*lsp.Symbol{sym("ConfigRetryTTL"), sym("Greeting"), sym("MaxGroupRate"), sym("Greet")}
	out := FormatWorkspaceSymbols(syms, "/r", "grt", 50, true)
	if !strings.Contains(out, "Greet") || !strings.Contains(out, "Greeting") {
		t.Fatalf("compact matches must stay:\n%s", out)
	}
	if strings.Contains(out, "ConfigRetryTTL") || strings.Contains(out, "MaxGroupRate") {
		t.Fatalf("scattered matches must be hidden:\n%s", out)
	}
	if !strings.Contains(out, "(2 weak matches hidden)") {
		t.Fatalf("hidden count missing:\n%s", out)
	}
}

func TestCamelInitialsMatch(t *testing.T) {
	if _, ok := looseSpan("MaxGroupRate", "mgr"); !ok {
		t.Fatal("initials must match")
	}
}

func TestFormatDiagnosticsDedupesKeepingError(t *testing.T) {
	mk := func(sev, col int) lsp.Diagnostic {
		return lsp.Diagnostic{Range: lsp.Range{Start: lsp.Position{Line: 10, Character: col}}, Severity: sev, Message: "no new variables on left side of :="}
	}
	out := FormatDiagnostics([]lsp.Diagnostic{mk(2, 3), mk(1, 1)}, 10)
	if strings.Count(out, "no new variables") != 1 || !strings.Contains(out, "[error") {
		t.Errorf("got %q", out)
	}
}

func TestFormatDiagnosticsSortedByPosition(t *testing.T) {
	mk := func(line, col int, msg string) lsp.Diagnostic {
		return lsp.Diagnostic{Range: lsp.Range{Start: lsp.Position{Line: line, Character: col}}, Severity: 1, Message: msg}
	}
	out := FormatDiagnostics([]lsp.Diagnostic{mk(9, 0, "third"), mk(2, 5, "second"), mk(2, 1, "first")}, 10)
	if a, b, c := strings.Index(out, "first"), strings.Index(out, "second"), strings.Index(out, "third"); !(a < b && b < c) {
		t.Errorf("want line/column order, got:\n%s", out)
	}
}
