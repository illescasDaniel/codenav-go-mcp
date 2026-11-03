package tools

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/index"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

func writeSrc(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// stubSymbols is what gopls answers for n fuzzy matches of "Serv", spread over files and lines.
func stubSymbols(root string, n int) []lsp.Symbol {
	out := make([]lsp.Symbol, 0, n)
	for i := 0; i < n; i++ {
		file := filepath.Join(root, fmt.Sprintf("pkg%d/a.go", i%7))
		out = append(out, lsp.Symbol{
			Name: fmt.Sprintf("NewServer%03d", i), Kind: 12,
			Location:       &lsp.Location{URI: lsp.PathToURI(file)},
			SelectionRange: &lsp.Range{Start: lsp.Position{Line: i * 3, Character: 5}},
		})
	}
	return out
}

// BUG-DET-001: the same index and the same gopls matches, in any order and with a capped (partial) list, give
// the same answer; the count is the index's.
func TestSearchAnswerIgnoresGoplsOrder(t *testing.T) {
	root := t.TempDir()
	var src strings.Builder
	src.WriteString("package hits\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&src, "\nfunc ServerOpt%02d() {}\n", i)
	}
	writeSrc(t, root, "hits/h.go", src.String())
	ix := index.New(root)
	if err := ix.Refresh(); err != nil {
		t.Fatal(err)
	}
	base := stubSymbols(root, 150)
	first := searchAnswer(root, ix, "Serv", base, true, nil, "", false)
	for seed := int64(0); seed < 25; seed++ {
		shuffled := append([]lsp.Symbol(nil), base...)
		rand.New(rand.NewSource(seed)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got := searchAnswer(root, ix, "Serv", shuffled, true, nil, "", false); got != first {
			t.Fatalf("seed %d differs:\n%s\n---\n%s", seed, got, first)
		}
	}
	if !strings.Contains(first, "… and 10 more (showing first 50)") {
		t.Errorf("the count is the index's 60 hits:\n%s", first)
	}
}

// A declaration the index and gopls both report is listed once. The //line directive moves the position a
// compiler would report, not the file's real line: the index and gopls both give the real line 4.
func TestSearchAnswerListsLineDirectiveDeclarationOnce(t *testing.T) {
	root := t.TempDir()
	abs := writeSrc(t, root, "lined/lined.go", "package lined\n\n//line other.go:500\nfunc Lined() {}\n")
	ix := index.New(root)
	if err := ix.Refresh(); err != nil {
		t.Fatal(err)
	}
	gopls := []lsp.Symbol{{Name: "Lined", Kind: 12, Location: &lsp.Location{URI: lsp.PathToURI(abs)},
		SelectionRange: &lsp.Range{Start: lsp.Position{Line: 3, Character: 5}}}}
	out := searchAnswer(root, ix, "Lined", gopls, false, nil, "", false)
	if n := strings.Count(out, "Lined  [function]"); n != 1 || !strings.Contains(out, "lined/lined.go:4:6") {
		t.Errorf("want one line at the real position lined/lined.go:4:6, got:\n%s", out)
	}
}

// BUG-SRCH-004 (with gopls): gopls answers a local's name only with weak matches, which are hidden. The answer
// still says nothing matched, and that locals are not indexed.
func TestSearchAnswerWeakOnlyNotFoundHintsLocals(t *testing.T) {
	root := t.TempDir()
	var gopls []lsp.Symbol
	// Letters t,m,p,v,a,r in order, spread over more than 3*len(query) characters: weak, not loose.
	for i, n := range []string{"t" + strings.Repeat("x", 15) + "mxpxvxaxr", "xt" + strings.Repeat("x", 12) + "mxpxvxaxr"} {
		gopls = append(gopls, lsp.Symbol{Name: n, Kind: 12,
			Location:       &lsp.Location{URI: lsp.PathToURI(filepath.Join(root, "w/w.go"))},
			SelectionRange: &lsp.Range{Start: lsp.Position{Line: i, Character: 5}}})
	}
	out := searchAnswer(root, nil, "tmpVar", gopls, false, nil, "", false)
	if !strings.HasPrefix(out, `No symbols matching "tmpVar".`) || !strings.Contains(out, "not in the symbol index") ||
		!strings.Contains(out, "weak match") || !strings.Contains(out, "hover, definition or references") {
		t.Errorf("weak-only answer for a local:\n%s", out)
	}
}

// BUG-SRCH-004: a not-found name that is a local variable says locals are not indexed and points at the position tools.
func TestSearchAnswerNotFoundPointsAtPositionTools(t *testing.T) {
	out := searchAnswer(t.TempDir(), nil, "tmpVar", nil, false, nil, "", false)
	if !strings.HasPrefix(out, `No symbols matching "tmpVar".`) || !strings.Contains(out, "not in the symbol index") || !strings.Contains(out, "hover, definition or references") {
		t.Errorf("not-found answer for a local:\n%s", out)
	}
}

// BUG-SRCH-005: query and name with different values are refused; the same value twice is fine.
func TestSearchQueryRefusesConflictingAliases(t *testing.T) {
	if _, err := searchQuery("Foo", "Bar"); err == nil || toolerr.IsNotFound(err) || !strings.Contains(err.Error(), "aliases") {
		t.Errorf("query=Foo name=Bar: want a refused input, got %v", err)
	}
	for _, c := range [][2]string{{"Foo", "Foo"}, {"Foo", ""}, {"", "Bar"}} {
		if _, err := searchQuery(c[0], c[1]); err != nil {
			t.Errorf("query=%q name=%q: %v", c[0], c[1], err)
		}
	}
	if q, _ := searchQuery("", "Bar"); q != "Bar" {
		t.Errorf("name alone: %q", q)
	}
}

// BUG-SCALE-001: an index that stopped at its file cap says so, on every answer.
func TestSearchAnswerSaysWhenIndexIsCapped(t *testing.T) {
	old := index.MaxFiles
	index.MaxFiles = 2
	defer func() { index.MaxFiles = old }()
	root := t.TempDir()
	for _, f := range []string{"a.go", "b.go", "c.go"} {
		writeSrc(t, root, f, "package p\n\nfunc Scale"+strings.TrimSuffix(f, ".go")+"() {}\n")
	}
	ix := index.New(root)
	if err := ix.Refresh(); err != nil {
		t.Fatal(err)
	}
	if !ix.Capped() {
		t.Fatal("3 files with MaxFiles 2 must cap the index")
	}
	if out := searchAnswer(root, ix, "Scale", nil, false, nil, "", false); !strings.Contains(out, "lower bounds") {
		t.Errorf("a capped index must be announced:\n%s", out)
	}
}

// A path filter that empties the answer says how the path was read.
func TestSearchAnswerSaysHowThePathWasRead(t *testing.T) {
	root := t.TempDir()
	out := searchAnswer(root, nil, "Serv", stubSymbols(root, 3), false, nil, "internal", false)
	if !strings.Contains(out, `No symbols matching "Serv" with the given kind/path filters (3 without them).`) ||
		!strings.Contains(out, `The path "internal" was read as a workspace-relative directory or file prefix.`) {
		t.Errorf("path filter answer:\n%s", out)
	}
}
