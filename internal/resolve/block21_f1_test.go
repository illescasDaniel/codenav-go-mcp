package resolve

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/illescasDaniel/codenav-go-mcp/internal/index"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// b21f1Cands lists n stub candidates of one name, in files chosen by file (nil: one file per candidate).
func b21f1Cands(root string, n int, file func(i int) string) []*lsp.Symbol {
	var out []*lsp.Symbol
	for i := 0; i < n; i++ {
		f := file(i)
		var loc *lsp.Location
		if f != "" {
			loc = &lsp.Location{URI: lsp.PathToURI(filepath.Join(root, f))}
		}
		out = append(out, &lsp.Symbol{
			Name:          "Shared",
			ContainerName: fmt.Sprintf("example.com/t/p%d", i),
			Location:      loc,
		})
	}
	return out
}

// Item 4 (cap note): when every candidate sits in one file, file_path cannot narrow the list, so the note says
// where they are and how to narrow them instead.
func TestBlock21F1_CapNoteWhenAllInOneFile(t *testing.T) {
	root := t.TempDir()
	cands := b21f1Cands(root, 60, func(int) string { return "shared.go" })
	lines := strings.Split(candidateLines(cands, root), "\n")
	last := lines[len(lines)-1]
	want := "  ... and 10 more; all are in shared.go; qualify the name (Type.Member) or pass line/column"
	if last != want {
		t.Errorf("last line = %q, want %q", last, want)
	}
	if strings.Contains(last, "pass file_path") {
		t.Errorf("the note tells the user to pass file_path, which cannot narrow one file: %q", last)
	}
}

// Item 4 (cap note): a candidate with no file cannot be narrowed by file_path, so the note does not offer it.
func TestBlock21F1_CapNoteWhenACandidateHasNoFile(t *testing.T) {
	root := t.TempDir()
	cands := b21f1Cands(root, 60, func(i int) string {
		if i == 59 {
			return "" // the last one is not in a file (empty URI)
		}
		return fmt.Sprintf("p%d/p.go", i)
	})
	lines := strings.Split(candidateLines(cands, root), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, "qualify the name (Type.Member) or pass line/column") || strings.Contains(last, "file_path") {
		t.Errorf("a candidate without a file must not be narrowed by file_path: %q", last)
	}
}

// Item 4 (cap note): several files keep the original advice, and the note counts the files over all candidates:
// here the first 50 are in one file, the rest elsewhere, so the list does not all sit in one file.
func TestBlock21F1_CapNoteCountsFilesOverAllCandidates(t *testing.T) {
	root := t.TempDir()
	cands := b21f1Cands(root, 60, func(i int) string {
		if i < 50 {
			return "same.go"
		}
		return "other.go"
	})
	lines := strings.Split(candidateLines(cands, root), "\n")
	last := lines[len(lines)-1]
	if want := "  ... and 10 more; pass file_path or a qualified name"; last != want {
		t.Errorf("last line = %q, want %q", last, want)
	}
}

// SCALE-002: merging the index's declarations with gopls's answer must not compare every declaration with every
// symbol so far (a name declared 20000 times took ~30 s). Here 20000 files each declare Shared on the same line,
// the case that made every pair compare file paths; no gopls answer, so every entry becomes a symbol. A
// quadratic merge is cut off by the guard.
func TestBlock21F1_MergeIsLinearInDeclarations(t *testing.T) {
	const n = 20000
	root := t.TempDir()
	for i := 0; i < n; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("s%d.go", i)), []byte("package big\n\nfunc Shared() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ix := index.New(root)
	if err := ix.Refresh(); err != nil {
		t.Fatal(err)
	}
	type result struct {
		exact, loose []*lsp.Symbol
		only         map[*lsp.Symbol]bool
	}
	done := make(chan result, 1)
	go func() {
		ex, lo, only := merge(ix, "Shared", nil, nil)
		done <- result{ex, lo, only}
	}()
	select {
	case r := <-done:
		if len(r.exact) != n || len(r.loose) != 0 || len(r.only) != n {
			t.Errorf("merge: %d exact, %d loose, %d index-only; want %d, 0, %d", len(r.exact), len(r.loose), len(r.only), n, n)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("merge of %d declarations did not finish in 10 s: it is quadratic again", n)
	}
}
