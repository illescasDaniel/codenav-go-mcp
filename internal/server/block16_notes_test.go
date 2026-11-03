package server

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// otherOS is a GOOS the test run is not on, so a _otherOS file is left out of the build.
func otherOS() string {
	if runtime.GOOS == "windows" {
		return "linux"
	}
	return "windows"
}

// DIAG-004: a file excluded by its name suffix gets the note, with the reason and how to include it.
func TestDiagnosticsNotesFileExcludedBySuffix(t *testing.T) {
	os := otherOS()
	cs, root := session(t, nil)
	writeFile(t, root, "d8/d8.go", "package d8\n\nfunc D() int { return 1 }\n")
	writeFile(t, root, "d8/win_"+os+".go", "package d8\n\nvar W int = \"s\"\n")
	out := call(t, cs, "diagnostics", map[string]any{"file_path": "d8/win_" + os + ".go"})
	mustHave(t, "suffix note", out, "This file is excluded from the current build (file name suffix _"+os+")", "GOOS="+os)
}

// DIAG-005: the note agrees with the errors shown (the file was checked on its own, not left out), and says how to
// include the file.
func TestDiagnosticsNoteAgreesWithErrorsOfExcludedFile(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "d8/notother.go", "//go:build !"+runtime.GOOS+"\n\npackage d8\n\nvar N int = \"s\"\n")
	out := call(t, cs, "diagnostics", map[string]any{"file_path": "d8/notother.go"})
	mustHave(t, "constraint note", out, "excluded from the current build (//go:build !"+runtime.GOOS+")", "To include it, start the server with the GOOS or GOARCH it needs")
	if strings.Contains(out, "not type-checked") {
		t.Errorf("the note contradicts the errors shown (the file was type-checked on its own):\n%s", out)
	}
}

// DIAG-005: a //go:build ignore file is a standalone program, so the note says to run it, not to include it.
func TestDiagnosticsNotesStandaloneFile(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "d8/gen.go", "//go:build ignore\n\npackage main\n\nfunc main() { var x int = \"s\"; _ = x }\n")
	out := call(t, cs, "diagnostics", map[string]any{"file_path": "d8/gen.go"})
	mustHave(t, "standalone note", out, "has //go:build ignore, so no build includes it", "go run")
}

// DIAG-004/005: a file the build includes gets no note (the old note named every file with a constraint).
func TestDiagnosticsNoNoteForIncludedFile(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "d8/own.go", "//go:build "+runtime.GOOS+"\n\npackage d8\n\nfunc Own() int { return 1 }\n")
	out := call(t, cs, "diagnostics", map[string]any{"file_path": "d8/own.go"})
	if strings.Contains(out, "excluded from the current build") || strings.Contains(out, "build-constrained") {
		t.Errorf("an included file gets no build note:\n%s", out)
	}
}

// NAVN-006: a caller in a file the build leaves out is not in the list, so "No callers found." gets the note.
func TestCallersNotesUseInExcludedFile(t *testing.T) {
	os := otherOS()
	cs, root := session(t, nil)
	writeFile(t, root, "p/lib.go", "package p\n\nfunc onWin() {}\n")
	writeFile(t, root, "p/x_"+os+".go", "package p\n\nfunc useWin() { onWin() }\n")
	out := call(t, cs, "callers", map[string]any{"name": "p.onWin"})
	mustHave(t, "callers note", out, "No callers found.", "1 use(s) of onWin in files the build leaves out are not in this list: p/x_"+os+".go:3 (file name suffix _"+os+")")
}

// ENV-010e: an edit that changes a func's signature names the caller in a file the build leaves out (the write is
// not held: it is a note, and it is applied).
func TestEditNotesExcludedCallerOfChangedFunc(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "tag/tag.go", "package tag\n\nfunc Helper(n int) int { return n }\n")
	writeFile(t, root, "tag/tag_integration.go", "//go:build integration\n\npackage tag\n\nfunc Caller() int { return Helper(1) }\n")
	out := call(t, cs, "edit", map[string]any{"file_path": "tag/tag.go", "old_string": "func Helper(n int) int", "new_string": "func Helper(n, m int) int"})
	mustHave(t, "edit applied with note", out, "Applied (edit", "Note: 1 use(s) of Helper in files the build leaves out were not checked: tag/tag_integration.go:5 (//go:build integration)")
	if got := readFile(t, root, "tag/tag.go"); !strings.Contains(got, "func Helper(n, m int) int") {
		t.Fatalf("the edit was not written:\n%s", got)
	}
}

// ENV-010e: an edit to a file the build leaves out says so; an edit to an included file with a constraint does not.
func TestEditNoteForConstrainedFileOnlyWhenExcluded(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "tag/tag_integration.go", "//go:build integration\n\npackage tag\n\nfunc Caller() int { return 1 }\n")
	writeFile(t, root, "tag/own.go", "//go:build "+runtime.GOOS+"\n\npackage tag\n\nfunc Own() int { return 1 }\n")
	out := call(t, cs, "edit", map[string]any{"file_path": "tag/tag_integration.go", "old_string": "return 1", "new_string": "return 2", "apply": false})
	mustHave(t, "excluded edited file", out, "Note: tag/tag_integration.go is excluded from the current build (//go:build integration)")
	out = call(t, cs, "edit", map[string]any{"file_path": "tag/own.go", "old_string": "return 1", "new_string": "return 2", "apply": false})
	if strings.Contains(out, "excluded from the current build") {
		t.Errorf("an included file gets no build note:\n%s", out)
	}
}

// Review: an edit to a function's body alone changes no signature, so an excluded caller gets no note.
func TestEditNoteSkipsBodyOnlyChange(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "tag/tag.go", "package tag\n\nfunc Helper(n int) int { return n }\n")
	writeFile(t, root, "tag/tag_integration.go", "//go:build integration\n\npackage tag\n\nfunc Caller() int { return Helper(1) }\n")
	out := call(t, cs, "edit", map[string]any{"file_path": "tag/tag.go", "old_string": "return n }", "new_string": "return n + 1 }"})
	if strings.Contains(out, "use(s) of Helper") {
		t.Errorf("a body-only edit gets no note about the excluded caller:\n%s", out)
	}
	if got := readFile(t, root, "tag/tag.go"); !strings.Contains(got, "return n + 1 }") {
		t.Fatalf("the edit was not written:\n%s", got)
	}
}

// Review: F declared in foo_<GOOS>.go and foo_<other>.go is one function per build. Editing F's signature in the
// included file must not say F is declared again in the excluded one, which cannot be built with it. The control
// G is declared in an excluded file that can be built with the edited one, so its note stays.
func TestEditNoteSkipsPlatformSplitDeclaration(t *testing.T) {
	cs, root := session(t, nil)
	included, excluded := "p/foo_"+runtime.GOOS+".go", "p/foo_"+otherOS()+".go"
	writeFile(t, root, included, "package p\n\nfunc F() int { return 1 }\n")
	writeFile(t, root, excluded, "package p\n\nfunc F() int { return 2 }\n")
	writeFile(t, root, "p/x.go", "package p\n\nfunc G() int { return 1 }\n")
	writeFile(t, root, "p/g_integration.go", "//go:build integration\n\npackage p\n\nfunc G() int { return 3 }\n")
	out := call(t, cs, "edit", map[string]any{"file_path": included, "old_string": "func F() int", "new_string": "func F() (int, error)", "apply": false})
	if strings.Contains(out, "F is declared again") {
		t.Errorf("a platform-split declaration is not declared again:\n%s", out)
	}
	out = call(t, cs, "edit", map[string]any{"file_path": "p/x.go", "old_string": "func G() int", "new_string": "func G() (int, error)", "apply": false})
	mustHave(t, "a declaration that can be built with the edited file", out, "G is declared again in files the build leaves out: p/g_integration.go:")
}

// Review: a large edit scans at most maxExcludedNames names. The note says how many of the changed names were
// checked, and a use of a name past the cap is not reported (the scan would otherwise match every name).
func TestEditNoteCapsTheNamesItScans(t *testing.T) {
	cs, root := session(t, nil)
	var src strings.Builder
	src.WriteString("package tag\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&src, "\nfunc N%02d() int { return %d }\n", i, i)
	}
	writeFile(t, root, "tag/tag.go", src.String())
	writeFile(t, root, "tag/tag_integration.go", "//go:build integration\n\npackage tag\n\nfunc Caller() int { return N00() + N55() }\n")
	out := call(t, cs, "edit", map[string]any{"file_path": "tag/tag.go", "old_string": "() int { return ", "new_string": "() int64 { return ", "replace_all": true, "apply": false})
	mustHave(t, "the cap note", out, "Uses in files the build leaves out were checked for 50 of the 60 changed names only", "1 use(s) of N00 in files the build leaves out")
	if strings.Contains(out, "use(s) of N55") {
		t.Errorf("a name past the cap is not scanned:\n%s", out)
	}
}
