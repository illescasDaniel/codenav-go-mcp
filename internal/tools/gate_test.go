package tools

import (
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/diagdelta"
)

// BED-001 / ENV-001 / SCALE-005: the gate says NOT CHECKED for every reason the check did not cover what it had to.
func TestGateReasons(t *testing.T) {
	cases := []struct {
		name       string
		health     string
		incomplete []string
		checked    int
		want       []string
	}{
		{"all covered", "", nil, 3, nil},
		{"health bad", "go: broken", nil, 3, []string{"the Go configuration does not load, so no package is loaded: go: broken"}},
		{"incomplete", "", []string{"the package of an edited file does not load (x)"}, 3, []string{"the package of an edited file does not load (x)"}},
		{"nothing checked", "", nil, 0, []string{"no Go file was type-checked"}},
		{"several", "bad", []string{"i1"}, 0, []string{"the Go configuration does not load, so no package is loaded: bad", "i1", "no Go file was type-checked"}},
	}
	for _, c := range cases {
		got := gateReasons(c.health, c.incomplete, c.checked)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// SCALE-005: a capped delta counts its new errors as a lower bound.
func TestNewErrorsTextCapped(t *testing.T) {
	d := diagdelta.Delta{New: []diagdelta.Entry{{Path: "a.go", Line: 1, Column: 1, Severity: diagdelta.ErrorSeverity, Message: "x"}}}
	if got := newErrorsText(d); got != "1 new error(s)" {
		t.Errorf("uncapped: got %q", got)
	}
	d.Capped = 2
	if got := newErrorsText(d); got != "at least 1 new error(s)" {
		t.Errorf("capped: got %q", got)
	}
}
