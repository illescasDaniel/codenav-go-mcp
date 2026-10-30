package mentions

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

func TestFind(t *testing.T) {
	root := t.TempDir()
	src := "package p\n\n// Area is documented.\nfunc Area() {}\n\nvar s = \"Area\"\n\nfunc g() { Area() }\n"
	_ = os.WriteFile(filepath.Join(root, "a.go"), []byte(src), 0o644)
	_ = os.WriteFile(filepath.Join(root, "README.md"), []byte("call Area here\nAreas are not\n"), 0o644)
	covered := CoveredBy(map[string][]lsp.TextEdit{filepath.Join(root, "a.go"): {{Range: lsp.Range{Start: lsp.Position{Line: 3, Character: 5}}}}})
	got := map[string]int{}
	for _, m := range Find(root, "Area", covered) {
		got[m.Kind]++
	}
	want := map[string]int{"code": 1, "comment": 1, "string": 1, "text": 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %d want %d (%v)", k, got[k], v, got)
		}
	}
}
