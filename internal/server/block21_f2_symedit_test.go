package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression (ESYM-005): a new file takes its package clause from a sibling the build includes, not from a
// //go:build ignore file that sorts first (package main here).
func TestBlock21F2_NewFileSkipsBuildIgnoredSibling(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "b21ign/ignored.go", "//go:build ignore\n\npackage main\n\nfunc main() {}\n")
	writeFile(t, root, "b21ign/lib.go", "package lib\n\nfunc Lib() int { return 1 }\n")
	out, isErr := callIsError(t, cs, "edit_symbol", map[string]any{"action": "insert", "file_path": "b21ign/new.go", "source": "func New() int { return 2 }"})
	if isErr {
		t.Fatalf("inserting into a new file must be written:\n%s", out)
	}
	if got := readFile(t, root, "b21ign/new.go"); !strings.HasPrefix(got, "package lib\n") {
		t.Fatalf("the new file must take package lib from the build-included sibling, got:\n%s", got)
	}
}

// Regression (ESYM-006): a comment-only source for a new file is refused as the existing-file branches refuse it;
// no file is created. Both spellings: bare comments, and a package clause followed by comments only.
func TestBlock21F2_CommentOnlySourceRefusedForNewFile(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "b21cm/keep.go", "package b21cm\n\nfunc Keep() {}\n")
	for _, source := range []string{"// only a comment\n", "package b21cm\n\n// only a comment\n"} {
		out, isErr := callIsError(t, cs, "edit_symbol", map[string]any{"action": "insert", "file_path": "b21cm/new.go", "source": source})
		if !isErr {
			t.Fatalf("comment-only source %q must be refused (isError):\n%s", source, out)
		}
		mustHave(t, "comment-only refusal", out, "source holds only comments; give a declaration.")
		if _, err := os.Stat(filepath.Join(root, "b21cm/new.go")); err == nil {
			t.Fatalf("a refused insert created the file (source %q)", source)
		}
	}
}

// Regression (ESYM-008): a type that embeds a qualified type from another package names it in the refusal for a
// member it does not declare, instead of the generic hint. The other package's contents are not read.
func TestBlock21F2_QualifiedEmbedNamedInRefusal(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "b21qe/inner/inner.go", "package inner\n\ntype Inner struct{}\n\nfunc (Inner) Other() int { return 1 }\n")
	writeFile(t, root, "b21qe/outer/outer.go", "package outer\n\nimport \"example.com/fx/b21qe/inner\"\n\ntype Outer struct{ inner.Inner }\n\nfunc Use() Outer { return Outer{} }\n")
	out, _ := callIsError(t, cs, "symbol_info", map[string]any{"name": "Outer.Hello"})
	mustHave(t, "qualified embed named", out, "`Outer` embeds `inner.Inner` (another package); `Hello` may be promoted from it: query `inner.Inner.Hello`.")
}

// Regression (NAVN-007 follow-up): a workspace symbol named C of any kind (here a function) keeps its normal answer
// for C.Something; only a name that no workspace declares gets the cgo refusal.
func TestBlock21F2_CgoGuardAnyWorkspaceSymbolC(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "b21cg/c.go", "package b21cg\n\nfunc C() int { return 1 }\n")
	out, _ := callIsError(t, cs, "symbol_info", map[string]any{"name": "C.Something"})
	if strings.Contains(out, "cgo name") {
		t.Fatalf("a workspace function C must not get the cgo refusal:\n%s", out)
	}
	mustHave(t, "C.Something answer", out, "No symbol found matching")
}

// Regression (ESYM-005 follow-up): when the only sibling is left out by a build constraint (here windows only), a new
// file takes that sibling's package clause, not the directory name. On a windows host the sibling is built and the
// clause is the same, so the test holds on every platform.
func TestBlock21F2_NewFileTakesWindowsOnlySiblingPackage(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "b21win/win.go", "//go:build windows\n\npackage winlib\n\nfunc W() int { return 1 }\n")
	out, isErr := callIsError(t, cs, "edit_symbol", map[string]any{"action": "insert", "file_path": "b21win/new.go", "source": "func New() int { return 2 }"})
	if isErr {
		t.Fatalf("inserting into a new file must be written:\n%s", out)
	}
	if got := readFile(t, root, "b21win/new.go"); !strings.HasPrefix(got, "package winlib\n") {
		t.Fatalf("the new file must take package winlib from the windows-only sibling, got:\n%s", got)
	}
}

// Regression (ESYM-005 follow-up): a sibling behind a custom build tag the build does not set (integration here) gives
// its package clause to a new file, as the windows-only sibling does.
func TestBlock21F2_NewFileTakesIntegrationTagSiblingPackage(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "b21tag/tag.go", "//go:build integration\n\npackage tagpkg\n\nfunc T() int { return 1 }\n")
	out, isErr := callIsError(t, cs, "edit_symbol", map[string]any{"action": "insert", "file_path": "b21tag/new.go", "source": "func New() int { return 2 }"})
	if isErr {
		t.Fatalf("inserting into a new file must be written:\n%s", out)
	}
	if got := readFile(t, root, "b21tag/new.go"); !strings.HasPrefix(got, "package tagpkg\n") {
		t.Fatalf("the new file must take package tagpkg from the integration-tagged sibling, got:\n%s", got)
	}
}

// Regression (ESYM-005 follow-up): the only sibling is an ignore-tagged script of package main in a directory that is
// not main. Its clause is no guide, so the new file takes the directory name, as before.
func TestBlock21F2_NewFileIgnoredMainSiblingNotTaken(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "b21im/script.go", "//go:build ignore\n\npackage main\n\nfunc main() {}\n")
	out, isErr := callIsError(t, cs, "edit_symbol", map[string]any{"action": "insert", "file_path": "b21im/new.go", "source": "func New() int { return 2 }"})
	if isErr {
		t.Fatalf("inserting into a new file must be written:\n%s", out)
	}
	if got := readFile(t, root, "b21im/new.go"); !strings.HasPrefix(got, "package b21im\n") {
		t.Fatalf("the new file must take the directory name, not package main from the ignored script, got:\n%s", got)
	}
}
