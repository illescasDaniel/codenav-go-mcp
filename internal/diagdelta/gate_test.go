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

// EDIT-001: include_dependents=false says the importers were not checked, next to the count, without holding the write.
func TestFormatDependentsOffSaysImportersNotChecked(t *testing.T) {
	d := Delta{FilesChecked: 1, DependentsOff: true}
	got := Format(d, 15)
	if !strings.Contains(got, "Diagnostics: 0 new error(s), 0 fixed (1 file(s) checked, 0 diagnostic(s) unchanged; importing packages NOT checked because include_dependents=false - run verify_changes(include_dependents=true) before relying on this)") {
		t.Errorf("dependents off: want the importers named as not checked, got:\n%s", got)
	}
	if strings.HasPrefix(got, "NOT CHECKED") {
		t.Errorf("dependents off is a disclosure, not a hold:\n%s", got)
	}
}

// LEAK-006: "unchanged" counts diagnostics; the label says so instead of reading as a file count.
func TestFormatUnchangedCountsDiagnostics(t *testing.T) {
	d := Delta{FilesChecked: 1, Unchanged: 1}
	if got := Format(d, 15); !strings.Contains(got, "(1 file(s) checked, 1 diagnostic(s) unchanged)") {
		t.Errorf("unchanged label: got:\n%s", got)
	}
}

// VERIFY-004: a capped check with no new errors does not read as an exact "0 new error(s)", nor "at least 0".
func TestNewCountTextCappedZero(t *testing.T) {
	if got := (Delta{Capped: 240}).NewCountText(); got != "0 new error(s) in the checked files" {
		t.Errorf("capped zero: got %q", got)
	}
	if got := (Delta{}).NewCountText(); got != "0 new error(s)" {
		t.Errorf("uncapped zero: got %q", got)
	}
}
