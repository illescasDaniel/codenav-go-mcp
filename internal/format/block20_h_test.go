package format

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// Regression (SCALE-004): a 200 KB single-line file gives a snippet of at most a few hundred characters, and
// the matched column stays visible.
func TestWideLineSnippetIsCutAroundColumn(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "wide.go")
	line := "var big = " + strings.Repeat("A", 100000) + " + Target()"
	src := "package wide\n\nfunc Target() int { return 1 }\n\n" + line + "\n"
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	col := strings.Index(line, "Target")
	loc := lsp.Location{
		URI: "file://" + p,
		Range: lsp.Range{
			Start: lsp.Position{Line: 4, Character: col},
			End:   lsp.Position{Line: 4, Character: col + len("Target")},
		},
	}
	out := FormatLocation(loc, dir)
	if len(out) >= 400 {
		t.Fatalf("snippet of a 100 KB line is %d bytes, want under 400:\n%.300s", len(out), out)
	}
	if !strings.Contains(out, "Target()") {
		t.Errorf("the matched column is not visible in the snippet:\n%s", out)
	}
	if !strings.Contains(out, "...") {
		t.Errorf("a cut line must be marked with ..., got:\n%s", out)
	}
}

// A short line is printed whole, as before.
func TestShortLineSnippetUnchanged(t *testing.T) {
	if got := clipLine("x := 1", 2); got != "x := 1" {
		t.Errorf("clipLine short = %q", got)
	}
}
