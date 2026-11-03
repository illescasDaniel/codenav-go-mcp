package server

import (
	"fmt"
	"strings"
	"testing"
)

// Regression tests for block 11 (search_symbol), run against gopls.

// BUG-DET-001 / BUG-SRCH-006: a declaration that a //line directive renames is listed once, at its real position,
// and the same answer comes back on every call.
func TestSearchSymbolLineDirectiveListedOnceAndStable(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "lined/lined.go", "package lined\n\n//line other.go:500\nfunc Lined() {}\n")
	first := call(t, cs, "search_symbol", map[string]any{"query": "Lined"})
	if n := strings.Count(first, "Lined  [function]"); n != 1 {
		t.Errorf("Lined listed %d times, want once:\n%s", n, first)
	}
	mustHave(t, "real position", first, "lined/lined.go:4:6")
	for i := 0; i < 2; i++ {
		if again := call(t, cs, "search_symbol", map[string]any{"query": "Lined"}); again != first {
			t.Errorf("call %d differs from the first:\n%s\n---\n%s", i+2, again, first)
		}
	}
}

// BUG-DET-001: with more fuzzy matches than gopls returns, the answer is the same on every call and the
// partial sample is labelled.
func TestSearchSymbolFuzzyCrowdIsStable(t *testing.T) {
	cs, root := session(t, nil)
	var b strings.Builder
	b.WriteString("package crowd2\n\n")
	for i := 0; i < 150; i++ {
		fmt.Fprintf(&b, "func NewServer%03d() {}\n", i)
	}
	writeFile(t, root, "crowd2/c.go", b.String())
	first := call(t, cs, "search_symbol", map[string]any{"query": "Serv"})
	mustHave(t, "partial sample label", first, "(partial sample: gopls returns at most 100 matches per query, so the fuzzy matches shown here are only a sample and are not counted; they may differ between runs)")
	for i := 0; i < 2; i++ {
		if again := call(t, cs, "search_symbol", map[string]any{"query": "Serv"}); again != first {
			t.Errorf("call %d differs from the first:\n%s\n---\n%s", i+2, again, first)
		}
	}
}
