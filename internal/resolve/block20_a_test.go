package resolve

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// SCALE-002: an ambiguity listing is capped at 50 candidate lines, with the total and the way to narrow it.
func TestCandidateLinesCapsTheList(t *testing.T) {
	root := t.TempDir()
	var cands []*lsp.Symbol
	for i := 0; i < 200; i++ {
		cands = append(cands, &lsp.Symbol{
			Name:          "Shared",
			ContainerName: fmt.Sprintf("example.com/t/p%d", i),
			Location: &lsp.Location{
				URI: lsp.PathToURI(filepath.Join(root, fmt.Sprintf("p%d", i), "p.go")),
			},
		})
	}
	lines := strings.Split(candidateLines(cands, root), "\n")
	if len(lines) != 51 {
		t.Fatalf("got %d lines for 200 candidates, want 50 candidates and one note", len(lines))
	}
	if last := lines[len(lines)-1]; last != "  ... and 150 more; pass file_path or a qualified name" {
		t.Errorf("last line = %q", last)
	}
	if few := strings.Split(candidateLines(cands[:3], root), "\n"); len(few) != 3 || strings.Contains(strings.Join(few, "\n"), "more") {
		t.Errorf("three candidates must be listed in full with no note: %q", few)
	}
}

// ENV-012: the case note is decided with both foldings the matches use: EqualFold (ſum finds Sum, which
// lower-casing does not) and strings.ToLower (the index takes "İndex", U+0130, to Index).
func TestFoldedFormUsesTheMatchFolding(t *testing.T) {
	for _, c := range []struct{ name, query, want string }{
		{"Index", "İndex", "Index"}, // the index's lower-casing (U+0130 becomes i)
		{"Index", "indEx", "Index"},
		{"Index", "INDEX", "Index"},
		{"Index", "Index", "Index"},
		{"Index", "Other", ""},
		{"Sum", "ſum", "Sum"}, // EqualFold: U+017F folds to s
		{"Sum", "SUM", "Sum"},
	} {
		if got := foldedForm(&lsp.Symbol{Name: c.name}, c.query); got != c.want {
			t.Errorf("foldedForm(%s, %q) = %q, want %q", c.name, c.query, got, c.want)
		}
	}
}
