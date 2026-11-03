package server

import (
	"strings"
	"testing"
)

// EDIT-009 neighbour, through the tool path: a real `#include <no_such_header.h>` on disk is reported at its line
// with gcc's text (gopls' own positioned form, which condenseCgo must leave alone). The edit path cannot show this
// error here: gopls does not run cgo on an overlay, so the edit check never sees the header (see the block 20I report).
func TestCgoHeaderErrorOnDiskKeepsItsLine(t *testing.T) {
	skipWithoutCgo(t)
	cs, root := session(t, nil)
	writeFile(t, root, "cg/cg.go", "package cg\n\n/*\n#include <no_such_header.h>\n*/\nimport \"C\"\n\nfunc C1() int {\n\treturn int(C.nothing())\n}\n")
	out := call(t, cs, "diagnostics", map[string]any{"file_path": "cg/cg.go"})
	if !strings.Contains(out, "4:10 [error go list] cgo: fatal error: no_such_header.h: No such file or directory") {
		t.Errorf("want the header error at 4:10:\n%s", out)
	}
}

// SHAPE-007 through the tool path: with GO111MODULE=off every answer's configuration warning names the GOPATH
// mode and the fix, not the go command's "list -m cannot be used" refusal.
func TestGOPATHModeWarningNamesTheFix(t *testing.T) {
	cs, _ := session(t, map[string]string{"GO111MODULE": "off"})
	out := call(t, cs, "workspace", map[string]any{})
	if !strings.Contains(out, "GOPATH mode is not supported by gopls; add a go.mod") {
		t.Errorf("want the GOPATH-mode reason in the warning:\n%s", out)
	}
	if strings.Contains(out, "list -m cannot be used") {
		t.Errorf("the go command's refusal must not be shown as the problem:\n%s", out)
	}
}

// DIAG-006 through the diagnostics tool: a cgo failure is shown as gcc's error at gcc's position (4:8), with the
// cgo prolog and wrapper code left out.
func TestCgoFailureDiagnosticShowsGccErrorAtItsPosition(t *testing.T) {
	skipWithoutCgo(t)
	cs, root := session(t, nil)
	writeFile(t, root, "d10/c.go", "package d10\n\n/*\nint f( {\n*/\nimport \"C\"\n\nfunc G() { C.f() }\n")
	out := call(t, cs, "diagnostics", map[string]any{"file_path": "d10/c.go"})
	if !strings.Contains(out, "4:8 [error go list] cgo: error: expected declaration specifiers or '...' before '{' token") {
		t.Errorf("want gcc's error at 4:8:\n%s", out)
	}
	for _, bad := range []string{"full error output", "#include <stddef.h>", "cgo: gcc did not produce error"} {
		if strings.Contains(out, bad) {
			t.Errorf("the cgo dump %q must be left out:\n%s", bad, out)
		}
	}
}
