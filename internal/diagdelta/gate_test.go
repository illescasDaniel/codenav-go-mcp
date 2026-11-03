package diagdelta

import (
	"strings"
	"testing"
)

// BED-001 / ENV-002: a delta that did not check what it had to never reads as a clean "0 new error(s)".
func TestFormatNotCheckedNeverReadsClean(t *testing.T) {
	d := Delta{NotChecked: []string{"the Go configuration does not load, so no package is loaded: go: x"}}
	got := Format(d, 15)
	if !strings.HasPrefix(got, "NOT CHECKED: ") || strings.Contains(got, "0 new error(s)") {
		t.Errorf("not checked delta: want a NOT CHECKED head and no clean count, got:\n%s", got)
	}

	d.New = []Entry{{Path: "a.go", Line: 1, Column: 1, Severity: ErrorSeverity, Message: "boom"}}
	got = Format(d, 15)
	if !strings.HasPrefix(got, "NOT CHECKED: ") || !strings.Contains(got, "1 new error(s) in the files that were checked") {
		t.Errorf("not checked delta with an error: want the NOT CHECKED head first and the error count after it, got:\n%s", got)
	}
}

// SCALE-005: a delta that left dependent files out reports its error count as a lower bound.
func TestFormatCappedCountIsAtLeast(t *testing.T) {
	d := Delta{New: []Entry{{Path: "a.go", Line: 1, Column: 1, Severity: ErrorSeverity, Message: "boom"}}, Capped: 3}
	if got := Format(d, 15); !strings.Contains(got, "Diagnostics: at least 1 new error(s)") {
		t.Errorf("capped delta: want \"at least\", got:\n%s", got)
	}
	d.Capped = 0
	if got := Format(d, 15); !strings.Contains(got, "Diagnostics: 1 new error(s)") {
		t.Errorf("uncapped delta: want the exact count, got:\n%s", got)
	}
}
