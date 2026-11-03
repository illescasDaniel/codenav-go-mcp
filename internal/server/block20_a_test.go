package server

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// NAVN-007 through the tool: symbol_info on a cgo name explains the cgo name instead of printing go list's text.
func TestCgoNameIsExplainedNotProbed(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "cgobed/cgo.go", "package cgobed\n\n/*\nstatic int add(int a, int b) { return a + b; }\n*/\nimport \"C\"\n\nfunc Add(a, b int) int { return int(C.add(C.int(a), C.int(b))) }\n")
	for _, q := range []string{"C.malloc", "C.int"} {
		out := squash(call(t, cs, "symbol_info", map[string]any{"name": q, "include_references": false}))
		if !strings.Contains(out, q+" is a cgo name, not a Go package symbol; use hover or definition at a use of it") {
			t.Errorf("symbol_info(%q) must explain the cgo name:\n%s", q, out)
		}
		if strings.Contains(out, "go list") || strings.Contains(out, "probe") {
			t.Errorf("symbol_info(%q) leaks internal probe text:\n%s", q, out)
		}
	}
	// A workspace type called C keeps its own answer for a missing member: C is then no cgo name.
	writeFile(t, root, "ctype/c.go", "package ctype\n\ntype C struct{ n int }\n")
	out := squash(call(t, cs, "symbol_info", map[string]any{"name": "C.Nope", "include_references": false}))
	if strings.Contains(out, "cgo name") || !strings.Contains(out, "type C declares no Nope") {
		t.Errorf("a workspace type C must keep its own answer:\n%s", out)
	}
}

// SCALE-002 through the tool: an ambiguity answer for a name shared by 60 packages lists 50 of them and says how many
// are left, instead of one line per candidate.
func TestAmbiguityListingIsCapped(t *testing.T) {
	cs, root := session(t, nil)
	for i := 0; i < 60; i++ {
		writeFile(t, root, fmt.Sprintf("capbed/p%02d/p.go", i), fmt.Sprintf("package p%02d\n\nfunc Shared20() int { return %d }\n", i, i))
	}
	out := call(t, cs, "symbol_info", map[string]any{"name": "Shared20", "include_references": false})
	header := regexp.MustCompile(`(\d+) symbols match "Shared20"`).FindStringSubmatch(out)
	if header == nil || header[1] != "60" {
		t.Fatalf("want the ambiguity answer for 60 candidates:\n%s", out)
	}
	listed := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "  ") && strings.Contains(line, "Shared20") {
			listed++
		}
	}
	if listed != 50 {
		t.Errorf("listed %d candidates, want 50:\n%s", listed, out)
	}
	if !strings.Contains(out, "... and 10 more; pass file_path or a qualified name") {
		t.Errorf("the cut list must say how many are left and how to narrow it:\n%s", out)
	}
}

// ENV-012 through the tool: "İndex20" (dotted capital I, which lower-cases to i) finds Index20 and gets the same
// case note as the ASCII variant "indEx20".
func TestCaseNoteForTurkishDottedI(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "ixbed/ix.go", "package ixbed\n\nfunc Index20() int { return 20 }\n")
	for _, q := range []string{"indEx20", "İndex20"} {
		out := squash(call(t, cs, "symbol_info", map[string]any{"name": q, "include_references": false}))
		want := fmt.Sprintf(`Note: no symbol is called "%s" (names are case-sensitive); this is Index20.`, q)
		if !strings.Contains(out, want) {
			t.Errorf("symbol_info(%q) must carry the case note:\n%s", q, out)
		}
	}
}

// ESYM-008 through the tool: a promoted method refusal names the embedded type that declares it, and writes nothing.
func TestPromotedMethodRefusalNamesEmbeddedType(t *testing.T) {
	cs, root := session(t, nil)
	const src = "package promobed\n\ntype Inner struct{}\n\nfunc (Inner) M() int { return 1 }\n\ntype Outer struct{ Inner }\n"
	writeFile(t, root, "promobed/p.go", src)
	out := squash(call(t, cs, "edit_symbol", map[string]any{"action": "replace", "name": "Outer.M", "file_path": "promobed/p.go", "source": "func (o Outer) M() int { return 2 }", "apply": false}))
	if !strings.Contains(out, "M is declared on Inner (embedded in Outer); use Inner.M") {
		t.Errorf("the refusal must name the embedded type:\n%s", out)
	}
	if got := readFile(t, root, "promobed/p.go"); got != src {
		t.Errorf("the refused edit wrote to the file:\n%s", got)
	}
}

// ESYM-008 when the type name is ambiguous: two packages each declare an Outer, and each embeds a different type that
// carries M. Which Outer is meant is unknown, so no embedded type is named and the general hint stands.
func TestPromotedMethodRefusalStaysGenericWhenTypeIsAmbiguous(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "ambone/p.go", "package ambone\n\ntype Inner struct{}\n\nfunc (Inner) M() int { return 1 }\n\ntype Outer struct{ Inner }\n")
	writeFile(t, root, "ambtwo/p.go", "package ambtwo\n\ntype Other struct{}\n\nfunc (Other) M() int { return 2 }\n\ntype Outer struct{ Other }\n")
	out := squash(call(t, cs, "symbol_info", map[string]any{"name": "Outer.M", "include_references": false}))
	if strings.Contains(out, "use Inner.M") || strings.Contains(out, "use Other.M") {
		t.Errorf("an ambiguous type name must not name one embedded owner:\n%s", out)
	}
	if !strings.Contains(out, "type Outer declares no M (methods promoted through embedding are not declared symbols; query the embedded type's method instead)") {
		t.Errorf("want the general hint for an ambiguous type:\n%s", out)
	}
}
