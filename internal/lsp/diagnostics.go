package lsp

import "strings"

// DedupeDiagnostics merges diagnostics that report one problem twice, keeping the most severe at its
// own position. gopls does this for a redeclaring ":=": a compiler error (code NoNewVar) over the
// whole statement plus a nonewvars analyzer warning over the ":=" with the same text. Two entries are
// merged when their messages match (ignoring case, spacing and a trailing period) and they start on
// the same line or their ranges overlap (a multi-line statement starts the two on different lines).
func DedupeDiagnostics(items []Diagnostic) []Diagnostic {
	rank := func(d Diagnostic) int {
		if d.Severity == 0 {
			return 99
		}
		return d.Severity
	}
	out := make([]Diagnostic, 0, len(items))
	keys := make([]string, 0, len(items))
next:
	for _, d := range items {
		k := diagKey(d.Message)
		for i, o := range out {
			if keys[i] == k && (o.Range.Start.Line == d.Range.Start.Line || overlaps(o.Range, d.Range)) {
				if rank(d) < rank(o) {
					out[i] = d
				}
				continue next
			}
		}
		out = append(out, d)
		keys = append(keys, k)
	}
	return out
}

func diagKey(msg string) string {
	return strings.TrimSuffix(strings.ToLower(strings.Join(strings.Fields(msg), " ")), ".")
}

func before(a, b Position) bool {
	return a.Line < b.Line || a.Line == b.Line && a.Character < b.Character
}

// overlaps reports whether two ranges share a character (end exclusive); an empty range counts when
// it touches the other.
func overlaps(a, b Range) bool {
	if a.Start == a.End || b.Start == b.End {
		return !before(a.End, b.Start) && !before(b.End, a.Start)
	}
	return before(a.Start, b.End) && before(b.Start, a.End)
}
