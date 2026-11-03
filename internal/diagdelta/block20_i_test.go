package diagdelta

import (
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// EDIT-009: an edit whose cgo preamble does not compile is refused with gcc's error at gcc's line, not as
// "error: # example.com/edit/cg" at 1:1.
func TestEditCgoHeaderErrorRendersGccLine(t *testing.T) {
	text := "package cg\n\n/*\n#include <no_such_header.h>\n*/\nimport \"C\"\n"
	msg := "# example.com/edit/cg\ncg/cg.go:4:10: fatal error: no_such_header.h: No such file or directory\n    4 | #include <no_such_header.h>\n      |          ^~~~~~~~~~~~~~~~~~\ncompilation terminated."
	items := []lsp.Diagnostic{{Range: lsp.Range{}, Severity: ErrorSeverity, Source: "go list", Message: msg}}
	es := FromLSP("cg/cg.go", text, items)
	if len(es) != 1 {
		t.Fatalf("one diagnostic, got %+v", es)
	}
	want := "cg/cg.go:4:10 error: cgo: fatal error: no_such_header.h: No such file or directory"
	if got := es[0].Render(); got != want {
		t.Errorf("want %q, got %q", want, got)
	}
	d := Diff(nil, es)
	if len(d.NewErrors()) != 1 {
		t.Errorf("the broken header is still a new error (the edit is refused): %+v", d)
	}
}

// Review: on the edit path, a gcc error in a C header is named by its file and does not move onto the Go file.
func TestEditCgoErrorInHeaderKeepsItsName(t *testing.T) {
	msg := "# example.com/edit/cg\ncgo: gcc did not produce error at completed:1\nfull error output:\nhelper.h:2:10: fatal error: x.h: No such file or directory"
	es := FromLSP("cg/cg.go", "package cg\n", []lsp.Diagnostic{{Severity: ErrorSeverity, Message: msg}})
	want := "cg/cg.go:1:1 error: cgo: helper.h:2:10: fatal error: x.h: No such file or directory"
	if len(es) != 1 || es[0].Render() != want {
		t.Errorf("want %q, got %+v", want, es)
	}
}

// DIAG-006 on the edit path: a cgo dump keeps only gcc's report; the package header is not repeated as the message.
func TestEditCgoDumpRendersOnlyGccReport(t *testing.T) {
	msg := "# example.com/diag/d10\ncgo: gcc did not produce error at completed:1\non input:\n\n#line 3 \"d10/c.go\"\n\nint f( {\n\nfull error output:\nd10/c.go:4:8: error: expected declaration specifiers or '...' before '{' token\n    4 | int f( {\n      |        ^"
	es := FromLSP("d10/c.go", "package d10\n", []lsp.Diagnostic{{Severity: ErrorSeverity, Message: msg}})
	if len(es) != 1 {
		t.Fatalf("one diagnostic, got %+v", es)
	}
	if got := es[0].Render(); !strings.HasPrefix(got, "d10/c.go:4:8 error: cgo: error: expected declaration specifiers") {
		t.Errorf("want gcc's error at 4:8, got %q", got)
	}
	if strings.Contains(es[0].Message, "# example.com") || strings.Contains(es[0].Message, "#line") {
		t.Errorf("the dump's internals must be left out: %q", es[0].Message)
	}
}
