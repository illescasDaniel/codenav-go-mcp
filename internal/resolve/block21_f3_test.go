package resolve

import (
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// BUG-LEAK-003 through probe: an unloadable package gets the go command's cause when there is one, and the generic
// sentence (no invented cause) when there is not.
func TestBlock21F3_ProbeReasonUsesImportCause(t *testing.T) {
	diags := []lsp.Diagnostic{{Message: `could not import example.com/dep (no required module provides package "example.com/dep")`}}
	target := probeTarget{importPath: "example.com/dep", rest: []string{"Dep"}}

	got := explainProbe(diags, target, "example.com/dep.Dep", func(p string) string {
		if p != "example.com/dep" {
			t.Errorf("cause asked for %q", p)
		}
		return "parsing dep/go.mod: dep/go.mod:7: syntax error"
	})
	want := `package "example.com/dep" cannot be loaded: parsing dep/go.mod: dep/go.mod:7: syntax error`
	if got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}

	got = explainProbe(diags, target, "example.com/dep.Dep", func(string) string { return "" })
	if !strings.Contains(got, "no required module provides it") || !strings.Contains(got, "add it to go.mod") {
		t.Errorf("unknown cause must keep the generic sentence, got %q", got)
	}
}
