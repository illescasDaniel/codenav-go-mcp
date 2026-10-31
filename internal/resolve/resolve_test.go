package resolve

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

func TestDedupeSymbolsAcrossSymlinkSpellings(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "dep.go"), []byte("package dep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	mk := func(p string, line int) *lsp.Symbol {
		return &lsp.Symbol{Name: "New", Location: &lsp.Location{URI: lsp.PathToURI(p), Range: lsp.Range{Start: lsp.Position{Line: line, Character: 5}}}}
	}
	got := dedupeSymbols([]*lsp.Symbol{mk(filepath.Join(real, "dep.go"), 4), mk(filepath.Join(link, "dep.go"), 4), mk(filepath.Join(link, "dep.go"), 9)})
	if len(got) != 2 {
		t.Errorf("same file, line and column through two spellings is one symbol; got %d", len(got))
	}
}
