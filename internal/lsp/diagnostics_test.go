package lsp

import (
	"encoding/json"
	"testing"
)

func diag(sl, sc, el, ec, sev int, src, code, msg string) Diagnostic {
	return Diagnostic{Range: Range{Start: Position{sl, sc}, End: Position{el, ec}}, Severity: sev, Source: src, Code: json.RawMessage(`"` + code + `"`), Message: msg}
}

const noNew = "no new variables on left side of :="

func TestDedupeDiagnosticsGoplsNoNewVars(t *testing.T) {
	// Raw pairs gopls returned (0-based here): same line, and a statement spanning two lines.
	for name, pair := range map[string][]Diagnostic{
		"same line":  {diag(4, 1, 4, 7, 1, "compiler", "NoNewVar", noNew), diag(4, 3, 4, 5, 2, "nonewvars", "default", noNew)},
		"multi line": {diag(4, 1, 5, 11, 1, "compiler", "NoNewVar", noNew), diag(5, 4, 5, 6, 2, "nonewvars", "default", noNew)},
		"warn first": {diag(5, 4, 5, 6, 2, "nonewvars", "default", noNew), diag(4, 1, 5, 11, 1, "compiler", "NoNewVar", noNew+".")},
	} {
		got := DedupeDiagnostics(pair)
		if len(got) != 1 || got[0].Severity != 1 || got[0].Source != "compiler" || got[0].Range.Start != (Position{4, 1}) {
			t.Errorf("%s: got %+v", name, got)
		}
	}
}

func TestDedupeDiagnosticsKeepsDistinct(t *testing.T) {
	items := []Diagnostic{
		diag(3, 1, 3, 2, 1, "compiler", "UndeclaredName", "undefined: x"),
		diag(7, 1, 7, 2, 1, "compiler", "UndeclaredName", "undefined: x"),        // another line, no overlap
		diag(3, 1, 3, 2, 1, "compiler", "UnusedVar", "declared and not used: y"), // other message, same spot
		diag(3, 4, 3, 5, 1, "compiler", "UnusedVar", "declared and not used: z"),
	}
	if got := DedupeDiagnostics(items); len(got) != 4 {
		t.Errorf("distinct problems merged: %+v", got)
	}
}
