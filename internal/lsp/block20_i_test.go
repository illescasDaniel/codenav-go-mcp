package lsp

import (
	"strings"
	"testing"
)

// cgoDump is gopls' message for a package whose preamble does not compile (recorded for DIAG-010 in qa/diag):
// the package header, cgo's generated prolog and wrapper, then gcc's own report after "full error output:".
const cgoDump = `# example.com/diag/d10
cgo: gcc did not produce error at completed:1
on input:

#line 1 "cgo-builtin-prolog"
#include <stddef.h>

/* Define intgo when compiling with GCC.  */
typedef ptrdiff_t intgo;

#define GO_CGO_GOSTRING_TYPEDEF
typedef struct { const char *p; intgo n; } _GoString_;
typedef struct { char *p; intgo n; intgo c; } _GoBytes_;
_GoString_ GoString(char *p);
_GoString_ GoStringN(char *p, int l);
_GoBytes_ GoBytes(void *p, int n);
char *CString(_GoString_);
void *CBytes(_GoBytes_);
void *_CMalloc(size_t);

__attribute__ ((unused))
static size_t _GoStringLen(_GoString_ s) { return (size_t)s.n; }

__attribute__ ((unused))
static const char *_GoStringPtr(_GoString_ s) { return s.p; }
#line 3 "d10/c.go"

int f( {

#line 1 "cgo-generated-wrapper"
#line 1 "not-declared"
void __cgo_f_1_1(void) { __typeof__(f) *__cgo_undefined__1; }
#line 1 "not-type"
void __cgo_f_1_2(void) { f *__cgo_undefined__2; }
#line 1 "not-int-const"
void __cgo_f_1_3(void) { enum { __cgo_undefined__3 = (f)*1 }; }
#line 1 "not-num-const"
void __cgo_f_1_4(void) { static const double __cgo_undefined__4 = (f); }
#line 1 "not-str-lit"
void __cgo_f_1_5(void) { static const char __cgo_undefined__5[] = (f); }
#line 1 "completed"
int __cgo__1 = __cgo__2;

full error output:
d10/c.go:4:8: error: expected declaration specifiers or '...' before '{' token
    4 | int f( {
      |        ^`

// DIAG-006 / EDIT-009: the dump is condensed to gcc's error at gcc's position, when the diagnosed file is known.
func TestCgoDumpIsCondensedToGccErrorAtItsPosition(t *testing.T) {
	out := CondenseCgo("d10/c.go", []Diagnostic{{Range: Range{}, Severity: 1, Source: "go list", Message: cgoDump}})
	if len(out) != 1 {
		t.Fatalf("one dump, one diagnostic: %+v", out)
	}
	d := out[0]
	if d.Range.Start != (Position{Line: 3, Character: 7}) {
		t.Errorf("want the gcc position 4:8 (0-based 3:7), got %+v", d.Range.Start)
	}
	if !strings.HasPrefix(d.Message, "cgo: error: expected declaration specifiers or '...' before '{' token\n") {
		t.Errorf("want the gcc error first, got:\n%s", d.Message)
	}
	for _, bad := range []string{"full error output", "#line", "cgo-builtin-prolog", "# example.com"} {
		if strings.Contains(d.Message, bad) {
			t.Errorf("the cgo internals %q must be left out:\n%s", bad, d.Message)
		}
	}
	if !strings.Contains(d.Message, "4 | int f( {") {
		t.Errorf("the source excerpt gcc printed is kept:\n%s", d.Message)
	}
}

// EDIT-009: a broken header in a cgo preamble is reported as gcc's fatal error at its line, not as the header line.
func TestCgoFatalHeaderErrorKeepsGccPosition(t *testing.T) {
	msg := "# example.com/edit/cg\ncgo: gcc did not produce error at completed:1\nfull error output:\ncg/cg.go:4:10: fatal error: no_such_header.h: No such file or directory\n    4 | #include <no_such_header.h>\n      |          ^"
	out := CondenseCgo("cg/cg.go", []Diagnostic{{Range: Range{}, Severity: 1, Message: msg}})
	if out[0].Range.Start != (Position{Line: 3, Character: 9}) {
		t.Errorf("want 4:10, got %+v", out[0].Range.Start)
	}
	if !strings.HasPrefix(out[0].Message, "cgo: fatal error: no_such_header.h: No such file or directory") {
		t.Errorf("want the fatal header error, got %q", out[0].Message)
	}
}

// A cgo dump gcc printed no error line for says so, with the dump's last line, instead of the dump.
func TestCgoDumpWithoutGccLineSaysCgoCouldNotRun(t *testing.T) {
	msg := "# example.com/x\ncgo: gcc did not produce error at completed:1\non input:\n\nint f( {\n\nfull error output:\ngcc: fatal error: no input\n"
	out := DedupeDiagnostics([]Diagnostic{{Severity: 1, Message: msg}})
	want := "cgo could not be run on this package: gcc: fatal error: no input"
	if out[0].Message != want {
		t.Errorf("want %q, got %q", want, out[0].Message)
	}
}

// Review: a build failure with a "# pkg" header and no cgo dump keeps every error line; none is dropped.
func TestBuildFailureWithHeaderKeepsEveryError(t *testing.T) {
	msg := "# p\na.go:3:4: undefined: x\na.go:5:1: error: weird"
	in := Diagnostic{Range: Range{Start: Position{Line: 2, Character: 3}}, Severity: 1, Message: msg}
	out := DedupeDiagnostics([]Diagnostic{in})
	if out[0].Message != msg || out[0].Range != in.Range {
		t.Errorf("a non-cgo message must be unchanged, got %q at %+v", out[0].Message, out[0].Range)
	}
	out = CondenseCgo("a.go", []Diagnostic{in})
	if out[0].Message != msg {
		t.Errorf("with the file known too, got %q", out[0].Message)
	}
}

// EDIT-009: gopls' load failure for a broken header is "# pkg" and gcc's report with no marker (the source is go list).
// The gcc error moves the diagnostic to its line; the same text from the compiler source is left alone.
func TestHeaderLoadFailureIsPositionedFromGoListSource(t *testing.T) {
	msg := "# example.com/edit/cg\ncg/cg.go:4:10: fatal error: no_such_header.h: No such file or directory\n    4 | #include <no_such_header.h>\n      |          ^~~~~~~~~~~~~~~~~~\ncompilation terminated."
	out := CondenseCgo("cg/cg.go", []Diagnostic{{Severity: 1, Source: "go list", Message: msg}})
	if out[0].Range.Start != (Position{Line: 3, Character: 9}) {
		t.Errorf("want 4:10, got %+v", out[0].Range.Start)
	}
	if !strings.HasPrefix(out[0].Message, "cgo: fatal error: no_such_header.h: No such file or directory\n") || !strings.Contains(out[0].Message, "compilation terminated.") {
		t.Errorf("want gcc's error with its excerpt, got %q", out[0].Message)
	}
	same := CondenseCgo("cg/cg.go", []Diagnostic{{Severity: 1, Source: "compiler", Message: msg}})
	if same[0].Message != msg || same[0].Range.Start.Line != 0 {
		t.Errorf("a compiler diagnostic must not be condensed: %+v", same[0])
	}
}

// Review: a gcc error in a C file or header keeps its file name, and the position stays where gopls put it.
func TestCgoErrorInOtherFileKeepsItsName(t *testing.T) {
	for _, gccFile := range []string{"foo.c", "helper.h"} {
		msg := "# example.com/diag/d10\ncgo: gcc did not produce error at completed:1\nfull error output:\n" + gccFile + ":3:4: error: bad thing\n    3 | x"
		in := Diagnostic{Range: Range{Start: Position{Line: 1, Character: 2}}, Severity: 1, Message: msg}
		out := CondenseCgo("d10/c.go", []Diagnostic{in})
		if out[0].Range != in.Range {
			t.Errorf("%s: the position must stay, got %+v", gccFile, out[0].Range)
		}
		if !strings.HasPrefix(out[0].Message, "cgo: "+gccFile+":3:4: error: bad thing") {
			t.Errorf("%s: the file name must be kept, got %q", gccFile, out[0].Message)
		}
	}
}

// Review: a gcc error in another Go file of the package is not placed on the diagnosed file.
func TestCgoErrorInOtherGoFileIsNotPlaced(t *testing.T) {
	msg := "# example.com/diag/d10\nfull error output:\nb.go:3:4: error: bad thing"
	in := Diagnostic{Range: Range{Start: Position{Line: 1, Character: 2}}, Severity: 1, Message: msg}
	out := CondenseCgo("a.go", []Diagnostic{in})
	if out[0].Range != in.Range || !strings.HasPrefix(out[0].Message, "cgo: b.go:3:4: error: bad thing") {
		t.Errorf("b.go's error must not move onto a.go: %+v %q", out[0].Range, out[0].Message)
	}
}

// Review: with several gcc errors, the one in the diagnosed file gives the position and the others stay in the text.
func TestCgoOwnFileErrorPositionedOthersKept(t *testing.T) {
	msg := "# example.com/diag/d10\nfull error output:\nfoo.c:1:1: error: first\nd10/c.go:4:8: error: second\n    4 | int f( {"
	out := CondenseCgo("d10/c.go", []Diagnostic{{Severity: 1, Message: msg}})
	if out[0].Range.Start != (Position{Line: 3, Character: 7}) {
		t.Errorf("want 4:8, got %+v", out[0].Range.Start)
	}
	if !strings.Contains(out[0].Message, "foo.c:1:1: error: first") || !strings.HasPrefix(out[0].Message, "cgo: error: second") {
		t.Errorf("the other error must be kept in the text:\n%s", out[0].Message)
	}
}

// Messages that are not a cgo dump are left as they are.
func TestNonCgoMessagesUnchanged(t *testing.T) {
	for _, msg := range []string{
		"undefined: x",
		"# example.com/x\nthis is not a gcc line",
		"go list failed to return CompiledGoFiles for \"d10\".\n    This may indicate failure to perform cgo processing.",
	} {
		out := DedupeDiagnostics([]Diagnostic{{Range: Range{Start: Position{Line: 2}}, Severity: 1, Message: msg}})
		if out[0].Message != msg || out[0].Range.Start.Line != 2 {
			t.Errorf("message %q must stay as it is, got %q at %+v", msg, out[0].Message, out[0].Range.Start)
		}
	}
}
