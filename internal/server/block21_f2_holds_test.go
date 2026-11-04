package server

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Regression (T1 follow-up): a held plan that is also over the file limit keeps its holds when apply_edit refuses
// it. The refusal names the holds it would write past, and the next apply_edit(allow_large=true) still restates
// them before Applied (before the fix, the stored plan had lost them after the first refusal).
func TestBlock21F2_HoldsSurviveOverLimitRefusal(t *testing.T) {
	cs, root := session(t, nil)
	const callers = 41 // with the declaration: 42 files, over the limit of 40
	const pair = "package rsigh\n\nvar n int\n\nfunc next() int { n++; return n }\n\nfunc Pair(a, b int) int { return a*10 + b }\n"
	writeFile(t, root, "rsigh/pair.go", pair)
	rels := []string{"rsigh/pair.go"}
	for i := 0; i < callers; i++ {
		rel := fmt.Sprintf("rsigh/c%02d.go", i)
		writeFile(t, root, rel, fmt.Sprintf("package rsigh\n\nfunc Run%02d() int { return Pair(next(), next()+100) }\n", i))
		rels = append(rels, rel)
	}
	before := b21f2Snapshot(t, root, rels)

	// The reorder holds (its calls evaluate arguments with side effects), so the first call is a preview.
	out, isErr := callIsError(t, cs, "change_signature", map[string]any{"name": "rsigh.Pair", "reorder": []string{"b", "a"}})
	if !isErr {
		t.Fatalf("a held reorder must be a refusal (isError):\n%s", out)
	}
	mustHave(t, "held reorder", out, "Not written", "The reorder changes the order in which", "allow_large=true")
	id := previewID(t, out)

	// apply_edit writes past the hold, but the file limit refuses it: nothing is written, and the refusal names the
	// hold it would have written past.
	out2, isErr2 := callIsError(t, cs, "apply_edit", map[string]any{"id": id})
	if !isErr2 {
		t.Fatalf("apply_edit without allow_large must refuse an over-limit preview:\n%s", out2)
	}
	mustHave(t, "over-limit refusal names the hold", out2, "touches 42 files", "Held (written past on apply_edit): The reorder changes the order in which")
	if got := b21f2Snapshot(t, root, rels); !reflect.DeepEqual(got, before) {
		t.Fatal("a refused apply_edit wrote files")
	}

	// The stored preview kept its holds: allow_large writes it and restates the hold before Applied.
	out3, isErr3 := callIsError(t, cs, "apply_edit", map[string]any{"id": id, "allow_large": true})
	if isErr3 {
		t.Fatalf("apply_edit(allow_large=true) must write the held preview:\n%s", out3)
	}
	held := strings.Index(out3, "Held, written anyway (as apply_edit was asked to): The reorder changes the order in which")
	applied := strings.Index(out3, "Applied (edit id")
	if held < 0 || applied < 0 || held > applied {
		t.Fatalf("the reply must restate the held reorder before Applied:\n%s", out3)
	}
	if got := readFile(t, root, "rsigh/pair.go"); !strings.Contains(got, "func Pair(b, a int) int") {
		t.Fatalf("apply_edit must reorder the declaration:\n%s", got)
	}
}
