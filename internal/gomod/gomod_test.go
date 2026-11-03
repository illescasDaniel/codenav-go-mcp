package gomod

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func module(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":      "module example.com/m\n\ngo 1.21\n",
		"lib/lib.go":  "package lib\n\nfunc F() {}\n",
		"app/app.go":  "package app\n",
		"app/tags.go": "//go:build integration\n\npackage app\n\nimport \"example.com/m/lib\"\n\nfunc G() { lib.F() }\n",
	})
	return root
}

func has(files []string, suffix string) bool {
	for _, f := range files {
		if strings.HasSuffix(f, suffix) {
			return true
		}
	}
	return false
}

func TestRelatedHonoursBuildFlags(t *testing.T) {
	root := module(t)
	lib := filepath.Join(root, "lib", "lib.go")
	t.Setenv(EnvBuildFlags, "")
	if files, _ := Related(context.Background(), root, []string{lib}); has(files, "app/tags.go") {
		t.Errorf("tagged importer must be invisible without the tag: %v", files)
	}
	t.Setenv(EnvBuildFlags, "-tags=integration")
	if files, _ := Related(context.Background(), root, []string{lib}); !has(files, "app/tags.go") {
		t.Errorf("tagged importer must be found with -tags=integration: %v", files)
	}
}

func TestListIsCachedUntilFilesChange(t *testing.T) {
	root := module(t)
	t.Setenv(EnvBuildFlags, "")
	ctx := context.Background()
	before := listRuns.Load()
	if _, err := list(ctx, root); err != nil {
		t.Fatal(err)
	}
	if _, err := list(ctx, root); err != nil {
		t.Fatal(err)
	}
	if n := listRuns.Load() - before; n != 1 {
		t.Fatalf("second call must hit the cache, go list ran %d times", n)
	}
	writeTree(t, root, map[string]string{"app/more.go": "package app\n\nimport \"example.com/m/lib\"\n\nfunc H() { lib.F() }\n"})
	pkgs, err := list(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if n := listRuns.Load() - before; n != 2 {
		t.Fatalf("a new file must invalidate the cache, go list ran %d times", n)
	}
	found := false
	for _, p := range pkgs {
		for _, f := range p.files() {
			found = found || strings.HasSuffix(f, "app/more.go")
		}
	}
	if !found {
		t.Error("stale result: new file missing")
	}
	t.Setenv(EnvBuildFlags, "-tags=integration")
	_, _ = list(ctx, root)
	if n := listRuns.Load() - before; n != 3 {
		t.Errorf("changed build flags must invalidate the cache, go list ran %d times", n)
	}
}

func TestRelatedInsideNestedModule(t *testing.T) {
	root := module(t)
	writeTree(t, root, map[string]string{
		"sub/go.mod":  "module example.com/sub\n\ngo 1.21\n",
		"sub/a/a.go":  "package a\n\nfunc A() {}\n",
		"sub/a/a2.go": "package a\n",
		"sub/b/b.go":  "package b\n\nimport \"example.com/sub/a\"\n\nfunc B() { a.A() }\n",
	})
	files, _ := Related(context.Background(), root, []string{filepath.Join(root, "sub", "a", "a.go")})
	if !has(files, "sub/a/a2.go") || !has(files, "sub/b/b.go") {
		t.Errorf("the nested module's own package files and importers must be related: %v", files)
	}
}

func TestRelatedWithCgoFiles(t *testing.T) {
	root := module(t)
	if out, err := exec.Command("go", "env", "CGO_ENABLED").Output(); err != nil || strings.TrimSpace(string(out)) != "1" {
		t.Skip("cgo is disabled")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		if _, err := exec.LookPath("gcc"); err != nil {
			t.Skip("no C compiler")
		}
	}
	writeTree(t, root, map[string]string{
		"cg/cg.go":    "package cg\n\n/*\nstatic int one(void) { return 1; }\n*/\nimport \"C\"\n\nfunc One() int { return int(C.one()) }\n",
		"cg/other.go": "package cg\n",
		"user/u.go":   "package user\n\nimport \"example.com/m/cg\"\n\nvar _ = cg.One\n",
	})
	ctx := context.Background()
	files, _ := Related(ctx, root, []string{filepath.Join(root, "cg", "cg.go")})
	if !has(files, "cg/other.go") || !has(files, "user/u.go") {
		t.Errorf("a cgo file's package siblings and importers must be related: %v", files)
	}
	files, _ = Related(ctx, root, []string{filepath.Join(root, "cg", "other.go")})
	if !has(files, "cg/cg.go") || !has(files, "user/u.go") {
		t.Errorf("a plain file must relate to the cgo file and the importers: %v", files)
	}
}

func TestHealth(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	ctx := context.Background()

	healthy := t.TempDir()
	writeTree(t, healthy, map[string]string{"go.mod": "module example.com/h\n\ngo 1.21\n", "h.go": "package h\n"})
	if p := Health(ctx, healthy); p != "" {
		t.Errorf("healthy module: %q", p)
	}

	healthyWork := t.TempDir()
	writeTree(t, healthyWork, map[string]string{
		"go.mod": "module example.com/w\n\ngo 1.21\n", "go.work": "go 1.21\n\nuse (\n\t.\n\t./sub\n)\n",
		"sub/go.mod": "module example.com/w/sub\n\ngo 1.21\n",
	})
	if p := Health(ctx, healthyWork); p != "" {
		t.Errorf("healthy go.work workspace: %q", p)
	}

	noMod := t.TempDir()
	if p := Health(ctx, noMod); p != "" {
		t.Errorf("a directory without any go.mod is not a problem: %q", p)
	}

	broken := t.TempDir()
	writeTree(t, broken, map[string]string{"go.mod": "this is not a go.mod\n"})
	p := Health(ctx, broken)
	if p == "" || strings.Contains(p, broken) || strings.Contains(p, "\n") {
		t.Errorf("garbled go.mod: want a one-line problem relative to the workspace, got %q", p)
	}
	// Cached answer follows the file: fixing it clears the problem.
	writeTree(t, broken, map[string]string{"go.mod": "module example.com/fixed\n\ngo 1.21\n\n// fixed and longer so the stamp changes\n"})
	if p := Health(ctx, broken); p != "" {
		t.Errorf("after fixing go.mod the problem must be gone: %q", p)
	}

	missingUse := t.TempDir()
	writeTree(t, missingUse, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.21\n", "go.work": "go 1.21\n\nuse (\n\t.\n\t./missing\n)\n",
	})
	p = Health(ctx, missingUse)
	if p == "" || !strings.Contains(p, "missing") || strings.Contains(p, missingUse) {
		t.Errorf("go.work with a missing use directory: %q", p)
	}
}

func TestWorkModules(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":       "module example.com/r\n\ngo 1.21\n",
		"go.work":      "go 1.21\n\nuse (\n\t.\n\t./zeta\n\t./alpha\n\t./nogomod\n\t./missing\n)\n",
		"zeta/go.mod":  "module example.com/zeta\n\ngo 1.21\n",
		"alpha/go.mod": "module example.com/alpha\n\ngo 1.21\n",
		"nogomod/x.go": "package x\n",
	})
	got := WorkModules(root)
	want := []string{filepath.Join(root, "alpha"), filepath.Join(root, "zeta")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("sorted use modules without root, dirs lacking a go.mod: got %v want %v", got, want)
	}
	// From a member module, the others (including the go.work's own directory module) are listed.
	got = WorkModules(filepath.Join(root, "alpha"))
	want = []string{root, filepath.Join(root, "zeta")}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("from member alpha: got %v want %v", got, want)
	}
	plain := t.TempDir()
	writeTree(t, plain, map[string]string{"go.mod": "module example.com/p\n\ngo 1.21\n"})
	if got := WorkModules(plain); len(got) != 0 {
		t.Errorf("no go.work: %v", got)
	}
}

// Round 12: several problems of one config are separated, not run together.
func TestHealthSeparatesErrors(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	two := t.TempDir()
	writeTree(t, two, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.21\n", "go.work": "go 1.21\n\nuse (\n\t.\n\t./gone1\n\t./gone2\n)\n",
	})
	p := Health(context.Background(), two)
	if !strings.Contains(p, "gone1/go.mod: no such file or directory; go: cannot load module gone2") {
		t.Errorf("two errors must be separated by \"; \": %q", p)
	}
}

// Round 13 (11f): a line that ends in ":" introduces the next one, so no ":;" shows up.
func TestHealthNoColonSemicolon(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	d := t.TempDir()
	writeTree(t, d, map[string]string{"go.mod": "this is not a go.mod\n"})
	p := Health(context.Background(), d)
	if strings.Contains(p, ":;") || !strings.Contains(p, "errors parsing go.mod: go.mod:1:") {
		t.Errorf("want \"errors parsing go.mod: go.mod:1: ...\", got %q", p)
	}
}
