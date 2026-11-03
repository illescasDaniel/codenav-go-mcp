package resolve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// NAVN-001 / NAVN-002: the receiver spellings of a method name the same symbol as the plain name.
func TestNormalizeQueryReceiverSpellings(t *testing.T) {
	for in, want := range map[string]string{
		"(*User).Hello":          "User.Hello",
		"*User.Hello":            "User.Hello",
		"(User).Hello":           "User.Hello",
		"List[T].Push":           "List.Push",
		"List[K, V].Push":        "List.Push",
		"p.List[Pair[K,V]].Push": "p.List.Push",
		"Foo[int]":               "Foo[int]",
		"*Foo":                   "*Foo",
		"Square.Area":            "Square.Area",
		"shapes.Scale":           "shapes.Scale",
	} {
		if got := normalizeQuery(in); got != want {
			t.Errorf("normalizeQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

// writeFiles creates the given files (relative path to text) under a fresh directory and returns it.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// NAVN-004 / LANG-001: a member named through a type alias resolves to the member of the type the alias names.
func TestAliasedMembersFindsMembersThroughAliases(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"p/alias.go": "package p\n\ntype Square struct{ side float64 }\n\ntype Sq = Square\n\nfunc (s Sq) Area() float64 { return s.side }\n",
		"p/chain.go": "package p\n\ntype A = B\n\ntype B = C\n\ntype C struct{ F int }\n",
	})
	sym := func(name, file string, line int) *lsp.Symbol {
		return &lsp.Symbol{Name: name, ContainerName: "example.com/t/p", Location: &lsp.Location{
			URI: lsp.PathToURI(filepath.Join(dir, file)), Range: lsp.Range{Start: lsp.Position{Line: line}}}}
	}
	area := sym("Sq.Area", "p/alias.go", 6)
	field := sym("C.F", "p/chain.go", 6)
	cands := []*lsp.Symbol{area, field, sym("Other.Area", "p/chain.go", 0)}

	for _, c := range []struct {
		query string
		want  []*lsp.Symbol
	}{
		{"Square.Area", []*lsp.Symbol{area}},   // the canonical name of the alias' type
		{"p.Square.Area", []*lsp.Symbol{area}}, // package-qualified
		{"A.F", []*lsp.Symbol{field}},          // two-step alias chain
		{"B.F", []*lsp.Symbol{field}},          // one-step alias
		{"Other.F", nil},                       // no such type
		{"q.Square.Area", nil},                 // another package
	} {
		got := aliasedMembers(cands, c.query)
		if len(got) != len(c.want) {
			t.Errorf("aliasedMembers(%q): %d symbols, want %d", c.query, len(got), len(c.want))
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("aliasedMembers(%q)[%d] = %s, want %s", c.query, i, got[i].Name, c.want[i].Name)
			}
		}
	}
}

// NAVN-014: a bare member name lists the members that carry it, instead of "not found".
func TestMemberNamedFindsQualifiedMembers(t *testing.T) {
	cands := []*lsp.Symbol{
		{Name: "Config.Name", ContainerName: "example.com/t/p"},
		{Name: "Server.Name", ContainerName: "example.com/t/p"},
		{Name: "Named", ContainerName: "example.com/t/p"},
		{Name: "pkg.Name", ContainerName: "example.com/t/p"},
	}
	got := memberNamed(cands, "Name")
	if len(got) != 3 {
		t.Fatalf("memberNamed(Name) = %d symbols, want Config.Name, Server.Name and pkg.Name", len(got))
	}
	err := memberRefusal("Name", got, "/nowhere")
	msg := err.Error()
	if !strings.Contains(msg, "Config.Name") || !strings.Contains(msg, "Server.Name") || !strings.Contains(msg, "Qualify it with its type") {
		t.Errorf("memberRefusal: %q", msg)
	}
	if strings.Contains(msg, "Named") {
		t.Errorf("a longer name is not a member called Name: %q", msg)
	}
}

// NAVN-010: a two-part query whose qualifier is also a standard-library package names both.
func TestQualifierAmbiguousNamesBothPackages(t *testing.T) {
	local := []*lsp.Symbol{{Name: "Handler", ContainerName: "example.com/httpbed/local/http"}}
	if got := qualifierAmbiguous("http.Handler", local); got != "net/http" {
		t.Errorf("http.Handler with a local http: %q, want net/http", got)
	}
	if got := qualifierAmbiguous("json.Marshal", []*lsp.Symbol{{Name: "Marshal", ContainerName: "example.com/jsonbed/local/json"}}); got != "encoding/json" {
		t.Errorf("json.Marshal with a local json: %q, want encoding/json", got)
	}
	if got := qualifierAmbiguous("http.Handler", []*lsp.Symbol{{Name: "Handler", ContainerName: "net/http"}}); got != "" {
		t.Errorf("the standard library's own package is not ambiguous: %q", got)
	}
	if got := qualifierAmbiguous("net/http.Handler", local); got != "" {
		t.Errorf("an import path is not ambiguous: %q", got)
	}
	err := qualifierRefusal("http.Handler", "net/http", local)
	if msg := err.Error(); !strings.Contains(msg, "net/http.Handler") || !strings.Contains(msg, "example.com/httpbed/local/http.Handler") {
		t.Errorf("qualifierRefusal must offer both paths: %q", msg)
	}
}

// keep the name of a lookup helper honest: a candidate with no location is never matched by alias.
func TestAliasedMembersSkipsSymbolsWithoutLocation(t *testing.T) {
	if got := aliasedMembers([]*lsp.Symbol{{Name: "Sq.Area"}}, "Square.Area"); got != nil {
		t.Errorf("no location, no match: %v", got)
	}
}
