package edits

import (
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

func edit(sl, sc, el, ec int, text string) lsp.TextEdit {
	return lsp.TextEdit{Range: lsp.Range{Start: lsp.Position{Line: sl, Character: sc}, End: lsp.Position{Line: el, Character: ec}}, NewText: text}
}

func TestApplyTextEditsUTF16AndCRLF(t *testing.T) {
	// "😀" is two UTF-16 units: column 3 is right after it.
	got, err := ApplyTextEdits("a😀b\r\nline2\r\n", []lsp.TextEdit{edit(0, 3, 0, 4, "X\nY"), edit(1, 0, 1, 4, "L")})
	if err != nil {
		t.Fatal(err)
	}
	if want := "a😀X\r\nY\r\nL2\r\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestApplyTextEditsOverlap(t *testing.T) {
	if _, err := ApplyTextEdits("abcdef", []lsp.TextEdit{edit(0, 0, 0, 4, ""), edit(0, 2, 0, 5, "")}); err == nil {
		t.Fatal("expected overlap error")
	}
}

func TestUnifiedDiffAndInverse(t *testing.T) {
	old, nw := "a\nb\nc\n", "a\nB\nc\n"
	c := FileChange{Path: "/r/x.go", Old: &old, New: &nw}
	d := UnifiedDiff(c, "/r", 1)
	for _, want := range []string{"--- a/x.go", "+++ b/x.go", "-b", "+B", "@@ -1,3 +1,3 @@"} {
		if !strings.Contains(d, want) {
			t.Errorf("diff missing %q:\n%s", want, d)
		}
	}
	if inv := c.Inverse(); *inv.New != old || *inv.Old != nw {
		t.Error("inverse wrong")
	}
}
