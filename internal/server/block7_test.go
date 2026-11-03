package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// gitRun runs git in dir with a fixed identity, failing the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gitBase makes dir a git repository whose HEAD holds everything currently in it.
func gitBase(t *testing.T, dir string) {
	t.Helper()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "base")
}

// VERIFY-001 / BUG-003: a new untracked Go file in a module below the git root is type-checked at its real path.
func TestVerifyChangesNewFileInModuleBelowGitRoot(t *testing.T) {
	top := t.TempDir()
	svc := filepath.Join(top, "svc")
	copyTree(t, "../../testdata/fixture", svc)
	writeFile(t, top, "README.md", "top\n")
	gitBase(t, top)
	cs, _ := sessionAt(t, svc)
	writeFile(t, svc, "shapes/zz_new.go", "package shapes\n\nfunc zzNew() int { return \"x\" }\n")
	out := call(t, cs, "verify_changes", map[string]any{})
	if !strings.Contains(out, "1 new error(s)") || !strings.Contains(out, "zz_new.go") {
		t.Fatalf("new file below the git root must be checked and its error reported:\n%s", out)
	}
}

// VERIFY-003b / BUG-003: a whole module that is untracked below the git root is checked, not counted as made-up paths.
func TestVerifyChangesFullyUntrackedModuleBelowGitRoot(t *testing.T) {
	top := t.TempDir()
	svc := filepath.Join(top, "svc")
	copyTree(t, "../../testdata/fixture", svc)
	writeFile(t, top, "README.md", "top\n")
	gitBase(t, top)
	cs, _ := sessionAt(t, svc)
	writeFile(t, svc, "shapes/zz_new.go", "package shapes\n\nfunc zzNew() int { return \"x\" }\n")
	out := call(t, cs, "verify_changes", map[string]any{})
	if !strings.Contains(out, "1 new error(s)") || !strings.Contains(out, "zz_new.go") {
		t.Fatalf("untracked module below the git root must report its error, not 0 new error(s):\n%s", out)
	}
}

// VERIFY-001 / VERIFY-005: a renamed Go file with an error is counted once, and the old file's declaration is not
// listed as a fixed diagnostic (the before-run must not see both declarations).
func TestVerifyChangesRenamedFileHasNoPhantomFix(t *testing.T) {
	const orig = "package rnm\n\nfunc A() int { return 1 }\n"
	const broken = "package rnm\n\nfunc A() int { return \"x\" }\n"
	t.Run("staged rename", func(t *testing.T) {
		cs, root := session(t, nil)
		writeFile(t, root, "rnm/a.go", orig)
		gitBase(t, root)
		gitRun(t, root, "mv", "rnm/a.go", "rnm/renamed.go")
		writeFile(t, root, "rnm/renamed.go", broken)
		out := call(t, cs, "verify_changes", map[string]any{})
		if !strings.Contains(out, "1 new error(s), 0 fixed") || strings.Contains(out, "DuplicateDecl") {
			t.Fatalf("renamed file: want 1 new error and no fixed entry:\n%s", out)
		}
	})
	t.Run("deleted and untracked", func(t *testing.T) {
		cs, root := session(t, nil)
		writeFile(t, root, "rnm/a.go", orig)
		gitBase(t, root)
		if err := os.Remove(filepath.Join(root, "rnm/a.go")); err != nil {
			t.Fatal(err)
		}
		writeFile(t, root, "rnm/renamed.go", broken)
		out := call(t, cs, "verify_changes", map[string]any{})
		if !strings.Contains(out, "1 new error(s), 0 fixed") || strings.Contains(out, "DuplicateDecl") {
			t.Fatalf("renamed file (old deleted, new untracked): want 1 new error and no fixed entry:\n%s", out)
		}
	})
}

// VERIFY-006: a new untracked file that redeclares a method of a tracked type is a new error, not "unchanged".
func TestVerifyChangesMethodRedeclaredInUntrackedFile(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "mt/t.go", "package mt\n\ntype T struct{}\n\nfunc (T) M() {}\n")
	gitBase(t, root)
	writeFile(t, root, "mt/aaa.go", "package mt\n\nfunc (T) M() {}\n")
	out := call(t, cs, "verify_changes", map[string]any{})
	if !strings.Contains(out, "1 new error(s)") {
		t.Fatalf("method redeclared in an untracked file: want 1 new error, the build fails:\n%s", out)
	}
}

// VERIFY-003: a deleted Go file is named, so the "N Go file(s)" count is explained (staged and unstaged deletion).
func TestVerifyChangesNamesDeletedGoFile(t *testing.T) {
	const importer = "package dlb\n\nimport \"example.com/fx/dl\"\n\nvar _ = dl.A\n"
	setup := func(t *testing.T) (*mcp.ClientSession, string) {
		cs, root := session(t, nil)
		writeFile(t, root, "dl/a.go", "package dl\n\nfunc A() int { return 1 }\n")
		writeFile(t, root, "dl/extra.go", "package dl\n\nfunc E() int { return 2 }\n")
		writeFile(t, root, "dlb/b.go", importer)
		gitBase(t, root)
		return cs, root
	}
	t.Run("staged", func(t *testing.T) {
		cs, root := setup(t)
		gitRun(t, root, "rm", "-q", "dl/a.go")
		out := call(t, cs, "verify_changes", map[string]any{})
		if !strings.Contains(out, "Deleted Go file(s): dl/a.go") || !strings.Contains(out, "1 new error(s)") {
			t.Fatalf("staged deletion: want the deleted file named and the importer error:\n%s", out)
		}
	})
	t.Run("unstaged", func(t *testing.T) {
		cs, root := setup(t)
		if err := os.Remove(filepath.Join(root, "dl/a.go")); err != nil {
			t.Fatal(err)
		}
		out := call(t, cs, "verify_changes", map[string]any{})
		if !strings.Contains(out, "Deleted Go file(s): dl/a.go") || !strings.Contains(out, "1 new error(s)") {
			t.Fatalf("unstaged deletion: want the deleted file named and the importer error:\n%s", out)
		}
	})
}

// VERIFY-005: when git is not on PATH the answer says so, instead of blaming the repository.
func TestVerifyChangesReportsMissingGit(t *testing.T) {
	cs, _ := session(t, nil)
	t.Setenv("PATH", t.TempDir())
	out, isErr := callIsError(t, cs, "verify_changes", map[string]any{})
	if !isErr || !strings.Contains(out, "not found on PATH") || strings.Contains(out, "not a git repository") {
		t.Fatalf("missing git: want an error naming the missing command, isError=%v:\n%s", isErr, out)
	}
}
