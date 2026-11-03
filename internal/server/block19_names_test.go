package server

import (
	"strings"
	"testing"
)

// squash joins the output's words with single spaces: the symbol lines pad their columns.
func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

// NAVN-004 / LANG-001 / NAVN-001 / BED-002 against gopls: the alias and receiver spellings resolve to the declared
// symbol, and a write tool explains an import alias instead of calling it an unknown package. The type names are
// unique to this test, so the fixture's own Square does not answer for them.
func TestNamesThroughAliasesAndReceivers(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "aliasbed/alias.go", "package aliasbed\n\ntype Zeta struct{ n int }\n\ntype Zt = Zeta\n\nfunc (z Zt) Tally() int { return z.n }\n\ntype Circle struct{ r float64 }\n\nfunc (c *Circle) Area() float64 { return c.r }\n")
	writeFile(t, root, "aliasbed/chain.go", "package aliasbed\n\ntype A = B\n\ntype B = C\n\ntype C struct{ F int }\n")
	writeFile(t, root, "aliasuse/use.go", "package aliasuse\n\nimport sv \"example.com/fx/shapes\"\n\nvar _ sv.Square\n")

	// NAVN-004: the canonical name of an alias receiver's method, plain and package-qualified.
	for _, q := range []string{"Zeta.Tally", "aliasbed.Zeta.Tally"} {
		if out := squash(call(t, cs, "symbol_info", map[string]any{"name": q, "include_references": false})); !strings.Contains(out, "Zt.Tally [method]") {
			t.Errorf("%s must resolve to the method declared on Zt:\n%s", q, out)
		}
	}
	// LANG-001: a field reached through a two-step alias chain, and through a one-step alias.
	for _, q := range []string{"A.F", "B.F"} {
		if out := squash(call(t, cs, "symbol_info", map[string]any{"name": q, "include_references": false})); !strings.Contains(out, "C.F [field]") {
			t.Errorf("%s must resolve to the field of C:\n%s", q, out)
		}
	}
	// NAVN-001: a pointer receiver spelling names the same method as the plain name.
	if out := squash(call(t, cs, "symbol_info", map[string]any{"name": "(*Circle).Area", "include_references": false})); !strings.Contains(out, "Circle.Area [method]") {
		t.Errorf("(*Circle).Area must resolve to Circle.Area:\n%s", out)
	}
	// BED-002: a write tool names an import alias as one, with the package name and the path to write.
	out := call(t, cs, "rename_symbol", map[string]any{"name": "sv.NoSuchName", "new_name": "X", "apply": false})
	if !strings.Contains(out, "sv is an import alias for example.com/fx/shapes") || !strings.Contains(out, "the package is shapes") {
		t.Errorf("a write tool must explain an import alias, not call it an unknown package:\n%s", out)
	}
}
