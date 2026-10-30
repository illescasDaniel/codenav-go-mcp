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

func TestUnifiedDiffInsertAtEndIsClean(t *testing.T) {
	old := "package p\n\nfunc A() {\n}\n"
	nw := old + "\nfunc B() {\n}\n"
	c := FileChange{Path: "/r/x.go", Old: &old, New: &nw}
	d := UnifiedDiff(c, "/r", 3)
	if strings.Contains(d, "-}") || strings.Contains(d, "No newline") {
		t.Fatalf("insert at end should not touch the old last line:\n%s", d)
	}
	if a, r := CountChanged(c); a != 3 || r != 0 {
		t.Fatalf("CountChanged = +%d -%d, want +3 -0\n%s", a, r, d)
	}
}

func TestUnifiedDiffFinalNewlineVisible(t *testing.T) {
	withNL, withoutNL := "a\nb\n}\n", "a\nb\n}"
	// Removing only the trailing newline.
	c := FileChange{Path: "/r/x.go", Old: &withNL, New: &withoutNL}
	d := UnifiedDiff(c, "/r", 3)
	want := "-}\n+}\n" + noNewlineMarker + "\n"
	if !strings.Contains(d, want) {
		t.Fatalf("removing the final newline not visible, want %q in:\n%s", want, d)
	}
	if a, r := CountChanged(c); a != 1 || r != 1 {
		t.Fatalf("CountChanged = +%d -%d, want +1 -1", a, r)
	}
	// Adding it back.
	d = UnifiedDiff(c.Inverse(), "/r", 3)
	if want := "-}\n" + noNewlineMarker + "\n+}\n"; !strings.Contains(d, want) {
		t.Fatalf("adding the final newline not visible, want %q in:\n%s", want, d)
	}
	// Appending after a last line that had no newline: that line changes, marked like git.
	nw := "a\nb\n}\nx\n"
	d = UnifiedDiff(FileChange{Path: "/r/x.go", Old: &withoutNL, New: &nw}, "/r", 3)
	if want := "-}\n" + noNewlineMarker + "\n+}\n+x\n"; !strings.Contains(d, want) {
		t.Fatalf("want %q in:\n%s", want, d)
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

func TestRestoreEOLMixed(t *testing.T) {
	old := "a\r\nb\nc\r\nd\n"
	if got, want := RestoreEOL("a\nB\nc\nd\n", old), "a\r\nB\nc\r\nd\n"; got != want {
		t.Errorf("same line count: got %q want %q", got, want)
	}
	if got, want := RestoreEOL("a\nx\ny\nc\nd\n", "a\r\nb\r\nc\r\nd\n"), "a\r\nx\r\ny\r\nc\r\nd\n"; got != want {
		t.Errorf("inserted lines: got %q want %q", got, want)
	}
	if got, want := RestoreEOL("a\nb\n", "a\r\nb\r\n"), "a\r\nb\r\n"; got != want {
		t.Errorf("uniform CRLF: got %q want %q", got, want)
	}
}
