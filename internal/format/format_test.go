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
	out := FormatWorkspaceSymbols(syms, "/r", "grt", 50, false)
	if !strings.Contains(out, "Greet") || !strings.Contains(out, "Greeting") {
		t.Fatalf("compact matches must stay:\n%s", out)
	}
	if strings.Contains(out, "ConfigRetryTTL") || strings.Contains(out, "MaxGroupRate") {
		t.Fatalf("scattered matches must be hidden:\n%s", out)
	}
	if !strings.Contains(out, "(2 weak matches hidden; pass fuzzy=true to list them)") {
		t.Fatalf("hidden count missing:\n%s", out)
	}
}

func TestFuzzyTrueListsWeakMatches(t *testing.T) {
	syms := []*lsp.Symbol{sym("ConfigRetryTTL"), sym("Greeting"), sym("MaxGroupRate"), sym("Greet")}
	out := FormatWorkspaceSymbols(syms, "/r", "grt", 50, true)
	if !strings.Contains(out, "ConfigRetryTTL") || !strings.Contains(out, "MaxGroupRate") || strings.Contains(out, "hidden") {
		t.Fatalf("fuzzy=true must list the weak matches:\n%s", out)
	}
	if strings.Index(out, "Greet") > strings.Index(out, "ConfigRetryTTL") {
		t.Fatalf("weak matches go last:\n%s", out)
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
	out := FormatDiagnostics([]lsp.Diagnostic{mk(2, 3), mk(1, 1)}, 10, "")
	if strings.Count(out, "no new variables") != 1 || !strings.Contains(out, "[error") {
		t.Errorf("got %q", out)
	}
}

func TestFormatDiagnosticsSortedByPosition(t *testing.T) {
	mk := func(line, col int, msg string) lsp.Diagnostic {
		return lsp.Diagnostic{Range: lsp.Range{Start: lsp.Position{Line: line, Character: col}}, Severity: 1, Message: msg}
	}
	out := FormatDiagnostics([]lsp.Diagnostic{mk(9, 0, "third"), mk(2, 5, "second"), mk(2, 1, "first")}, 10, "")
	if a, b, c := strings.Index(out, "first"), strings.Index(out, "second"), strings.Index(out, "third"); !(a < b && b < c) {
		t.Errorf("want line/column order, got:\n%s", out)
	}
}

// BUG-DIAG-003: an analyzer finding (code "default") shows the analyzer's name, a compiler code is kept as it is.
func TestFormatDiagnosticNamesTheAnalyzer(t *testing.T) {
	for _, c := range []struct {
		name string
		d    lsp.Diagnostic
		want string
	}{
		{"analyzer", lsp.Diagnostic{Severity: 2, Code: []byte(`"default"`), Source: "printf", Message: "x"}, "[warning printf]"},
		{"no code", lsp.Diagnostic{Severity: 2, Source: "unreachable", Message: "x"}, "[warning unreachable]"},
		{"compiler code", lsp.Diagnostic{Severity: 1, Code: []byte(`"UndeclaredName"`), Source: "compiler", Message: "x"}, "[error UndeclaredName]"},
	} {
		if got := FormatDiagnostic(c.d, ""); !strings.Contains(got, c.want) {
			t.Errorf("%s: want %q in %q", c.name, c.want, got)
		}
	}
}

// BUG-DIAG-008: the other declaration of a redeclared name is shown workspace-relative, not as a file:// URI.
func TestFormatDiagnosticShowsRelatedLocation(t *testing.T) {
	d := lsp.Diagnostic{
		Range:    lsp.Range{Start: lsp.Position{Line: 4, Character: 5}},
		Severity: 1,
		Code:     []byte(`"DuplicateDecl"`),
		Message:  "dup redeclared in this block",
		RelatedInformation: []lsp.DiagnosticRelated{{
			Location: lsp.Location{URI: "file:///root/d1/f07.go", Range: lsp.Range{Start: lsp.Position{Line: 31, Character: 5}}},
			Message:  "other declaration of dup",
		}},
	}
	out := FormatDiagnostic(d, "/root")
	want := "5:6 [error DuplicateDecl] dup redeclared in this block\n    d1/f07.go:32:6: other declaration of dup"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
	if strings.Contains(out, "file://") {
		t.Errorf("URI leaked: %q", out)
	}
	// An empty related message leaves the location alone, without a dangling ": ".
	d.RelatedInformation[0].Message = ""
	if out := FormatDiagnostic(d, "/root"); !strings.HasSuffix(out, "\n    d1/f07.go:32:6") {
		t.Errorf("empty related message: %q", out)
	}
}

// BUG-DIAG-002: when the same diagnostics report an import cycle, gopls' "missing metadata" wording for the import
// says it is part of that cycle; without a cycle the wording is left alone.
func TestExplainCycleRewritesMissingMetadata(t *testing.T) {
	const missing = `could not import example.com/diag/d4b (missing metadata for import of "example.com/diag/d4b")`
	cycle := lsp.Diagnostic{Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 7}}, Severity: 1, Message: "import cycle not allowed"}
	imp := lsp.Diagnostic{Range: lsp.Range{Start: lsp.Position{Line: 2, Character: 7}}, Severity: 1, Message: missing}
	out := FormatDiagnostics([]lsp.Diagnostic{cycle, imp}, 10, "")
	if strings.Contains(out, "missing metadata") || !strings.Contains(out, "its package is part of an import cycle") {
		t.Errorf("cycle wording: %s", out)
	}
	if out := FormatDiagnostics([]lsp.Diagnostic{imp}, 10, ""); !strings.Contains(out, "missing metadata") {
		t.Errorf("without a cycle the wording stays: %s", out)
	}
}

// LEAK-006: a compact references list says why it dropped the snippets.
func TestCompactReferenceListSaysWhy(t *testing.T) {
	var locs []lsp.Location
	for i := 0; i < 9; i++ {
		locs = append(locs, lsp.Location{URI: "file:///r/a.go", Range: lsp.Range{Start: lsp.Position{Line: i}, End: lsp.Position{Line: i}}})
	}
	out := FormatReferences(locs, "/r", 8)
	if !strings.Contains(out, "(compact list: 9 > 8 hits, so they are grouped by file and line with no source snippets)") {
		t.Errorf("compact label: got:\n%s", out)
	}
}
