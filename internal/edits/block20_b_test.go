package edits

import (
	"strings"
	"testing"
)

// EDIT-010: a refused diff cuts a line longer than the shown maximum, so a 200 KB line costs a few hundred bytes.
func TestBlock20BDiffCutsLongLine(t *testing.T) {
	long := "var s = \"" + strings.Repeat("a", 200000) + "\""
	old := "package long\n\n" + long + "\n"
	nw := "package long\n\nvar t = undefinedQ\n" + long + "\n"
	plan := NewPlan("edit long/long.go", []FileChange{{Path: "/r/long/long.go", Old: &old, New: &nw}})
	out := plan.RefusedDiff("/r", 2, 120)
	if len(out) > 2000 {
		t.Fatalf("diff is %d bytes; the long line should be cut", len(out))
	}
	if !strings.Contains(out, "more characters)") || !strings.Contains(out, "+var t = undefinedQ") {
		t.Errorf("want the cut line with its count and the short added line, got %q", out)
	}
}

// ClipLine counts runes, not bytes, and leaves a line of exactly the maximum alone.
func TestBlock20BClipLine(t *testing.T) {
	exact := strings.Repeat("é", maxShownLine)
	if got := ClipLine(exact); got != exact {
		t.Errorf("exact maximum: changed to %d runes", len([]rune(got)))
	}
	over := strings.Repeat("é", maxShownLine+7)
	want := strings.Repeat("é", maxShownLine) + "... (7 more characters)"
	if got := ClipLine(over); got != want {
		t.Errorf("over by 7: got %q want %q", got, want)
	}
}
