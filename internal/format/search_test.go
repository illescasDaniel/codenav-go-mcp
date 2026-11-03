package format

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

// symIn is a function-kind symbol named name, declared at line (0-based) of the workspace file rel.
func symIn(rel, name string, kind, line int) *lsp.Symbol {
	return &lsp.Symbol{
		Name: name, Kind: kind,
		Location:       &lsp.Location{URI: "file:///r/" + rel},
		SelectionRange: &lsp.Range{Start: lsp.Position{Line: line, Character: 5}},
	}
}

func names(syms []*lsp.Symbol, root string) []string {
	out := make([]string, len(syms))
	for i, s := range syms {
		out[i] = Relative(symFile(s), root)
	}
	return out
}

// BUG-SRCH-002 / BUG-SRCH-006: exact-name ties are ranked by package (production, then helpers, then tests,
// declarations before fields, outside internal/ first, shallower first), whatever order gopls answered in.
func TestRankPutsWorkspaceRootFirstAmongExactTies(t *testing.T) {
	syms := []*lsp.Symbol{
		symIn("internal/testutils/fakegrpclb/server.go", "NewServer", 12, 81),
		symIn("reflection/serverreflection.go", "NewServer", 12, 135),
		symIn("internal/testutils/xds/fakeserver/server.go", "NewServer", 12, 147),
		symIn("health/server.go", "NewServer", 12, 52),
		symIn("internal/xds/clients/internal/testutils/fakeserver/server.go", "NewServer", 12, 147),
		symIn("server.go", "NewServer", 12, 711),
	}
	want := []string{
		"server.go",
		"health/server.go",
		"reflection/serverreflection.go",
		"internal/testutils/fakegrpclb/server.go",
		"internal/testutils/xds/fakeserver/server.go",
		"internal/xds/clients/internal/testutils/fakeserver/server.go",
	}
	for seed := int64(0); seed < 20; seed++ {
		shuffled := append([]*lsp.Symbol(nil), syms...)
		rand.New(rand.NewSource(seed)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got := names(Rank(shuffled, "/r", "NewServer"), "/r"); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("seed %d: order\n%v\nwant\n%v", seed, got, want)
		}
	}
}

func TestRankDeclarationsBeforeFieldsAndTestsLast(t *testing.T) {
	syms := []*lsp.Symbol{
		symIn("a_test.go", "Serve", 12, 1),
		symIn("b.go", "Server.Serve", 6, 2),
		symIn("c.go", "Serve", 12, 3),
		symIn("d.go", "Field.Serve", 8, 4),
	}
	got := names(Rank(syms, "/r", "Serve"), "/r")
	want := []string{"b.go", "c.go", "d.go", "a_test.go"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("order %v, want %v", got, want)
	}
}

// BUG-DET-001 / BUG-SRCH-006: a list of 150 gopls matches in any order gives the same answer, byte for byte.
// The matches are genuine ties (same tier, package depth and test level) spread over files and lines.
func tiedStub() []*lsp.Symbol {
	var out []*lsp.Symbol
	for i := 0; i < 150; i++ {
		out = append(out, symIn(fmt.Sprintf("pkg%d/a.go", i%7), fmt.Sprintf("NewServer%03d", i), 12, i*3))
	}
	return out
}

func TestPartialSampleOutputIgnoresGoplsOrder(t *testing.T) {
	base := tiedStub()
	first := SearchSymbols(nil, base, true, "/r", "Serv", 50, false)
	for seed := int64(0); seed < 25; seed++ {
		shuffled := append([]*lsp.Symbol(nil), base...)
		rand.New(rand.NewSource(seed)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got := SearchSymbols(nil, shuffled, true, "/r", "Serv", 50, false); got != first {
			t.Fatalf("seed %d differs from the first answer:\n%s\n---\n%s", seed, got, first)
		}
	}
	if !strings.Contains(first, "(partial sample: gopls returns at most 100 matches per query") {
		t.Errorf("a partial sample must be labelled:\n%s", first)
	}
	if strings.Contains(first, " more (showing first") {
		t.Errorf("a partial sample is not counted:\n%s", first)
	}
}

// The count is taken from the exact and prefix hits; a partial gopls sample fills only the free slots.
func TestCountComesFromHitsOnly(t *testing.T) {
	var hits []*lsp.Symbol
	for i := 0; i < 60; i++ {
		hits = append(hits, symIn(fmt.Sprintf("hit%d.go", i), fmt.Sprintf("ServerOpt%02d", i), 12, i))
	}
	out := SearchSymbols(hits, tiedStub(), true, "/r", "Serv", 50, false)
	if !strings.Contains(out, "… and 10 more (showing first 50)") {
		t.Errorf("count must be the hits' (60 - 50):\n%s", out)
	}
	if strings.Contains(out, "NewServer") {
		t.Errorf("the sample must not take a slot from full hits:\n%s", out)
	}
	few := SearchSymbols(hits[:3], tiedStub(), true, "/r", "Serv", 50, false)
	if strings.Contains(few, " more (showing first") {
		t.Errorf("3 hits and a sample fill the list without an overflow count:\n%s", few)
	}
	if !strings.Contains(few, "NewServer000") {
		t.Errorf("the sample fills the free slots:\n%s", few)
	}
}

// A complete gopls list (fewer than the cap) is counted as before.
func TestCompleteSampleIsCounted(t *testing.T) {
	out := SearchSymbols(nil, tiedStub()[:60], false, "/r", "Serv", 50, false)
	if !strings.Contains(out, "… and 10 more (showing first 50)") || strings.Contains(out, "partial sample") {
		t.Errorf("a complete sample is counted:\n%s", out)
	}
}

// BUG-SRCH-003: a subsequence-only match is marked; a prefix match is not.
func TestSubsequenceMatchesAreMarked(t *testing.T) {
	out := FormatWorkspaceSymbols([]*lsp.Symbol{sym("Greet"), sym("Greeting"), sym("ConfigRetryTTL")}, "/r", "grt", 50, true)
	for _, name := range []string{"Greet", "ConfigRetryTTL"} {
		var line string
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, name+"  [") {
				line = l
			}
		}
		if !strings.HasSuffix(line, "(fuzzy)") {
			t.Errorf("%s is a subsequence-only match and must be marked:\n%s", name, out)
		}
	}
	prefix := FormatWorkspaceSymbols([]*lsp.Symbol{sym("Greeting")}, "/r", "Gree", 50, true)
	if strings.Contains(prefix, "(fuzzy)") {
		t.Errorf("a prefix match is not marked:\n%s", prefix)
	}
}

// BUG-SRCH-007 / BUG-SRCH-008: a glob without a slash matches base names; a plain path is a directory prefix.
func TestPathMatchesBoundaryAndGlobs(t *testing.T) {
	for _, c := range []struct {
		rel, pattern string
		want         bool
	}{
		{"p/a.go", "p", true},
		{"p/a.go", "p/", true},
		{"plugins/p1/p.go", "p", false},
		{"server.go", "server", false},
		{"server.go", "server.go", true},
		{"internal/x/y.go", "internal", true},
		{"internalfoo/y.go", "internal", false},
		{"stats/metricsregistry_test.go", "*_test.go", true},
		{"a/b/c_test.go", "*_test.go", true},
		{"a/b/c.go", "*_test.go", false},
		{"a/b/c_test.go", "**/*_test.go", true},
		{"c_test.go", "**/*_test.go", true},
		{"internal/x.go", "internal/*.go", true},
		{"internal/sub/x.go", "internal/*.go", false},
	} {
		if got := pathMatches(c.rel, c.pattern); got != c.want {
			t.Errorf("pathMatches(%q, %q) = %v, want %v", c.rel, c.pattern, got, c.want)
		}
	}
}

// BUG-SRCH-001: an absolute path or a `..` spelling that lands inside the workspace is read as the relative
// path; one that lands outside is refused as an input error (not a not-found answer).
func TestSearchPathNormalisesAndRefuses(t *testing.T) {
	const root = "/work/grpc"
	for in, want := range map[string]string{
		"":                         "",
		"internal":                 "internal",
		"internal/":                "internal",
		"./internal":               "internal",
		"/work/grpc/internal":      "internal",
		"../grpc/internal":         "internal",
		"/work/grpc":               "",
		"*_test.go":                "*_test.go",
		"**/*_test.go":             "**/*_test.go",
		"/work/grpc/internal/*.go": "internal/*.go",
	} {
		got, err := SearchPath(root, in)
		if err != nil || got != want {
			t.Errorf("SearchPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"/usr/lib/go/src/net", "../other", "../../x"} {
		_, err := SearchPath(root, bad)
		if err == nil || toolerr.IsNotFound(err) || !strings.Contains(err.Error(), "outside the workspace") {
			t.Errorf("SearchPath(%q): want a refused input error, got %v", bad, err)
		}
	}
}
