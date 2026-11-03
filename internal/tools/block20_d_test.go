package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
)

const b20dMod = "module example.com/mv\n\ngo 1.21\n"

// b20dTree writes files (slash-separated paths relative to root) below root.
func b20dTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, text := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func b20dRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// b20dMoveAcross plans the move of the declaration name from file (relative to root) to dest (relative to root),
// a file of another package. The plan is not applied.
func b20dMoveAcross(t *testing.T, root, file, name, dest string) (*edits.EditPlan, error) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(file))
	l := locateText(t, abs, b20dRead(t, abs), name)
	return moveAcrossPackages(context.Background(), &Workspace{Root: root}, l, filepath.Join(root, filepath.FromSlash(dest)), name, "")
}

// b20dMoveWithin plans the move of name from file into dest, a file of the same directory. The plan is not applied.
func b20dMoveWithin(t *testing.T, file, name, dest string) *edits.EditPlan {
	t.Helper()
	l := locateText(t, file, b20dRead(t, file), name)
	plan, err := moveWithinPackage(l, dest, "", false)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// b20dApply writes a plan the way apply_edit does for changed files (the fixtures here delete nothing).
func b20dApply(t *testing.T, plan *edits.EditPlan) {
	t.Helper()
	for _, c := range plan.Changes {
		if c.New == nil {
			t.Fatalf("unexpected deletion of %s", c.Path)
		}
		if err := os.WriteFile(c.Path, []byte(*c.New), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// BUG-LANG-004: an unexported func moved to another package that a declaration staying behind still uses is refused
// up front, naming that declaration (here a package-level var initializer).
func TestMoveRefusesUnexportedUsedByVarInitializer(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{
		"go.mod":         b20dMod,
		"linit/init.go":  "package linit\n\nvar cfg = load()\n\nfunc Cfg() int { return cfg }\n",
		"linit/load.go":  "package linit\n\nfunc load() int { return 42 }\n",
		"other/other.go": "package other\n\nfunc Other() int { return 1 }\n",
	})
	_, err := b20dMoveAcross(t, root, "linit/load.go", "load", "other/load.go")
	if err == nil {
		t.Fatal("moving the unexported load that cfg still calls must be refused")
	}
	for _, want := range []string{"`load` is unexported and still used by `cfg`", "export it or move cfg too"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must contain %q, got: %v", want, err)
		}
	}
}

// BUG-LANG-004: the same refusal when the stay-behind use is inside a function body.
func TestMoveRefusesUnexportedUsedInFunctionBody(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{
		"go.mod":         b20dMod,
		"linit/init.go":  "package linit\n\nfunc Cfg() int { return load() + 1 }\n",
		"linit/load.go":  "package linit\n\nfunc load() int { return 42 }\n",
		"other/other.go": "package other\n\nfunc Other() int { return 1 }\n",
	})
	_, err := b20dMoveAcross(t, root, "linit/load.go", "load", "other/load.go")
	if err == nil || !strings.Contains(err.Error(), "`load` is unexported and still used by `Cfg`") {
		t.Fatalf("want a refusal naming Cfg, got: %v", err)
	}
}

// Control for BUG-LANG-004: an exported func used by the code that stays behind is still moved (qualified).
func TestMoveAllowsExportedUsedByStayBehind(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{
		"go.mod":         b20dMod,
		"linit/init.go":  "package linit\n\nvar cfg = Load()\n\nfunc Cfg() int { return cfg }\n",
		"linit/load.go":  "package linit\n\nfunc Load() int { return 42 }\n",
		"other/other.go": "package other\n\nfunc Other() int { return 1 }\n",
	})
	if _, err := b20dMoveAcross(t, root, "linit/load.go", "Load", "other/load.go"); err != nil {
		t.Fatalf("an exported move must not be refused: %v", err)
	}
}

// BUG-MOVE-003: after a package-directory move, the places that still name the old import path (a Makefile, a
// Dockerfile, a //go:generate line, a goreleaser ldflags entry, testdata) are listed as not rewritten. The Go
// importer is rewritten and not listed.
func TestStaleImportNotesListNonGoMentions(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{
		"go.mod":                         b20dMod,
		"m017/lib/lib.go":                "//go:generate go run example.com/mv/m017/lib/gen\n\npackage lib\n\nfunc Version() int { return 1 }\n",
		"m017/main.go":                   "package main\n\nimport \"example.com/mv/m017/lib\"\n\nfunc main() { _ = lib.Version() }\n",
		".goreleaser.yml":                "builds:\n  - ldflags:\n      - -X example.com/mv/m017/lib.Version=1\n",
		"Dockerfile":                     "FROM scratch\nCOPY example.com/mv/m017/lib /lib\n",
		"Makefile":                       "build:\n\tgo build example.com/mv/m017/lib\n",
		"m012/imp00/testdata/golden.txt": "import example.com/mv/m012/pkg\n",
	})
	libGo := filepath.Join(root, "m017/lib/lib.go")
	mainGo := filepath.Join(root, "m017/main.go")
	// The state the move plans: lib.go moves to library/lib.go and main.go imports the new path.
	after := map[string]string{
		libGo:  b20dRead(t, libGo),
		mainGo: strings.Replace(b20dRead(t, mainGo), "example.com/mv/m017/lib\"", "example.com/mv/m017/library\"", 1),
	}
	renamed := map[string]string{libGo: filepath.Join(root, "m017/library/lib.go")}
	w := &Workspace{Root: root}

	got := strings.Join(staleImportNotes(w, "example.com/mv/m017/lib", "example.com/mv/m017/library", after, renamed), "\n")
	for _, want := range []string{
		"still mention the old import path example.com/mv/m017/lib (not rewritten)",
		"m017/library/lib.go:1", "Makefile:2", "Dockerfile:2", ".goreleaser.yml:3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("notes must contain %q, got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "main.go") {
		t.Errorf("the rewritten importer must not be listed, got:\n%s", got)
	}

	td := strings.Join(staleImportNotes(w, "example.com/mv/m012/pkg", "", nil, nil), "\n")
	if !strings.Contains(td, "m012/imp00/testdata/golden.txt:1") {
		t.Errorf("testdata mention must be listed, got:\n%s", td)
	}
}

// BUG-MOVE-005: an indirect import cycle is refused with its path (a -> b -> a), not with raw gopls errors.
// a imports b; b calls c.CFunc; moving CFunc into a would make b import a.
func TestMoveIndirectCycleNamesPath(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{
		"go.mod":      b20dMod,
		"m001/a/a.go": "package a\n\nimport \"example.com/mv/m001/b\"\n\nfunc A() int { return b.B() }\n",
		"m001/b/b.go": "package b\n\nimport \"example.com/mv/m001/c\"\n\nfunc B() int { return c.CFunc() }\n",
		"m001/c/c.go": "package c\n\nfunc CFunc() int { return 1 }\n\nfunc Other() int { return 2 }\n",
	})
	_, err := b20dMoveAcross(t, root, "m001/c/c.go", "CFunc", "m001/a/cfunc.go")
	want := "example.com/mv/m001/a -> example.com/mv/m001/b -> example.com/mv/m001/a"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want a refusal naming the cycle %q, got: %v", want, err)
	}
}

// BUG-MOVE-006: a destination directory whose name gives no valid Go package name is refused with the cause.
func TestMoveToNonIdentifierDirectoryRefused(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{
		"go.mod":          b20dMod,
		"m005/src/s.go":   "package src\n\nfunc Hello() string { return \"hi\" }\n\nfunc Keep() int { return 2 }\n",
		"m005/app/app.go": "package app\n\nimport \"example.com/mv/m005/src\"\n\nfunc Run() string { return src.Hello() }\n",
	})
	_, err := b20dMoveAcross(t, root, "m005/src/s.go", "Hello", "m005/123/u.go")
	if err == nil || !strings.Contains(err.Error(), "directory `123` gives no valid Go package name; choose another directory") {
		t.Fatalf("want the package-name refusal, got: %v", err)
	}
	// Control: a directory whose name is a valid identifier is still accepted.
	if _, err := b20dMoveAcross(t, root, "m005/src/s.go", "Hello", "m005/v2/u.go"); err != nil {
		t.Fatalf("a valid destination directory must not be refused: %v", err)
	}
}

// BUG-DET-002: moving a declaration out of a file and back leaves the file the first move created holding only its
// package clause. The move does not delete files, so the answer says the file is left empty (no silent stray file).
func TestMoveThereAndBackNotesEmptiedFile(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{
		"go.mod":     b20dMod,
		"lib/lib.go": "package lib\n\nfunc Greet() string { return \"hi\" }\n\nfunc Add(a, b int) int { return a + b }\n\nfunc Value() int { return 1 }\n",
	})
	lib := filepath.Join(root, "lib", "lib.go")
	extra := filepath.Join(root, "lib", "extra.go")

	b20dApply(t, b20dMoveWithin(t, lib, "Add", extra))
	back := b20dMoveWithin(t, extra, "Add", lib)
	b20dApply(t, back)

	if got := strings.TrimSpace(b20dRead(t, extra)); got != "package lib" {
		t.Fatalf("extra.go must be left with its package clause only, got %q", got)
	}
	if !strings.Contains(strings.Join(back.Notes, "\n"), "extra.go is left with nothing but its package clause") {
		t.Errorf("the answer must say extra.go was left empty, notes: %q", back.Notes)
	}
	if got := b20dRead(t, lib); !strings.Contains(got, "func Add(a, b int) int") {
		t.Fatalf("the declaration must be back in lib.go:\n%s", got)
	}
}

// BUG-DET-002, cross-package: the source file a move leaves with only its package clause is named in the answer.
func TestMoveAcrossPackagesNotesEmptiedSource(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{
		"go.mod":         b20dMod,
		"lib/extra.go":   "package lib\n\nfunc Add(a, b int) int { return a + b }\n",
		"other/other.go": "package other\n\nfunc Other() int { return 1 }\n",
	})
	plan, err := b20dMoveAcross(t, root, "lib/extra.go", "Add", "other/add.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(plan.Notes, "\n"), "extra.go is left with nothing but its package clause") {
		t.Errorf("the answer must say extra.go was left empty, notes: %q", plan.Notes)
	}
}

// Control for BUG-DET-002: a source file that keeps other declarations gets no such note.
func TestMoveKeepsSourceWithRemainingDeclarations(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{
		"go.mod":     b20dMod,
		"lib/lib.go": "package lib\n\nfunc Greet() string { return \"hi\" }\n\nfunc Add(a, b int) int { return a + b }\n",
	})
	lib := filepath.Join(root, "lib", "lib.go")
	plan := b20dMoveWithin(t, lib, "Add", filepath.Join(root, "lib", "extra.go"))
	if strings.Contains(strings.Join(plan.Notes, "\n"), "left with nothing") {
		t.Errorf("lib.go still holds Greet, no emptied-file note expected, notes: %q", plan.Notes)
	}
	b20dApply(t, plan)
	if got := b20dRead(t, lib); !strings.Contains(got, "func Greet()") {
		t.Fatalf("lib.go must keep Greet:\n%s", got)
	}
}

// BUG-MOVE-003 (review): a rewritten import of x/lib-x is not a stale mention of x/lib, and a hidden directory (an IDE
// or cache folder) is not scanned for leftovers.
func TestStaleImportNotesIgnoreNewPathAndHiddenDirs(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{
		"go.mod":              b20dMod,
		"usepkg/use.go":       "package use\n\nimport \"example.com/mv/lib-x\"\n",
		"Makefile":            "build:\n\tgo build example.com/mv/lib-x\n\tgo test example.com/mv/lib\n",
		".idea/workspace.xml": "<option value=\"example.com/mv/lib\"/>\n",
	})
	got := strings.Join(staleImportNotes(&Workspace{Root: root}, "example.com/mv/lib", "example.com/mv/lib-x", nil, nil), "\n")
	if !strings.Contains(got, "Makefile:3") {
		t.Errorf("the stale Makefile line must be listed, got:\n%s", got)
	}
	for _, not := range []string{"Makefile:2", "use.go", "workspace.xml"} {
		if strings.Contains(got, not) {
			t.Errorf("%s must not be listed, got:\n%s", not, got)
		}
	}
}

// The leftover scan says when it stops at its file cap instead of skipping the rest of the tree silently.
func TestStaleImportNotesNoteWhenScanStops(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{"go.mod": b20dMod}
	for i := 0; i < staleMaxFiles+1; i++ {
		files[fmt.Sprintf("bulk/f%04d", i)] = "x\n"
	}
	b20dTree(t, root, files)
	got := strings.Join(staleImportNotes(&Workspace{Root: root}, "example.com/mv/lib", "", nil, nil), "\n")
	if !strings.Contains(got, "stopped early") {
		t.Errorf("a scan that hit the file cap must say so, got:\n%s", got)
	}
}
