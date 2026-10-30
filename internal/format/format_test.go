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
