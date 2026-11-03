package gomod

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ENV-001: with no go command on PATH the health says so, instead of reporting nothing wrong.
func TestHealthGoMissingFromPath(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"go.mod": "module example.com/p\n\ngo 1.21\n"})
	t.Setenv("PATH", t.TempDir())
	if p := Health(context.Background(), root); p != noGoMsg {
		t.Errorf("no go on PATH: want %q, got %q", noGoMsg, p)
	}
}

// ENV-002: a GOWORK that does not exist, or is relative, is a configuration problem named as such.
func TestHealthGOWORKMissingOrRelative(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, map[string]string{"go.mod": "module example.com/g\n\ngo 1.21\n"})

	t.Setenv("GOWORK", filepath.Join(root, "missing.work"))
	if p := Health(ctx, root); p == "" {
		t.Errorf("GOWORK naming a missing file: want a problem")
	}

	t.Setenv("GOWORK", "relative.work")
	if p := Health(ctx, root); !strings.Contains(p, "GOWORK") {
		t.Errorf("relative GOWORK: want the problem to name GOWORK, got %q", p)
	}
}

// The health answer is cached per content, not per modification time: a same-size edit whose mtime is put
// back must still be seen.
func TestHealthFollowsContentNotMtime(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, map[string]string{"go.mod": "module example.com/a\n\ngo 1.21\n"})
	gm := filepath.Join(root, "go.mod")
	st, err := os.Stat(gm)
	if err != nil {
		t.Fatal(err)
	}
	if p := Health(ctx, root); p != "" {
		t.Fatalf("healthy go.mod: %q", p)
	}
	// Same size, different directive; the old modification time restored.
	writeTree(t, root, map[string]string{"go.mod": "module example.com/a\n\nfoo 1.21\n"})
	if err := os.Chtimes(gm, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if p := Health(ctx, root); p == "" {
		t.Errorf("same-size go.mod edit with the old mtime: the health must follow the content, got healthy")
	}
}

// BED-001 / SCALE-005: a dependency with no go.sum entry makes its package fail to load; RelatedLoad reports it
// instead of returning the importers as if the load were complete.
func TestRelatedLoadReportsMissingGoSum(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod": "module example.com/s\n\ngo 1.21\n\nrequire example.com/dep v1.0.0\n",
		"s.go":   "package s\n\nimport _ \"example.com/dep\"\n",
	})
	_, problems, err := RelatedLoad(context.Background(), root, []string{filepath.Join(root, "s.go")})
	if err == nil && len(problems) == 0 {
		t.Errorf("missing go.sum entry: the load problem must be reported (no error, no problems)")
	}
}

// BED-001: a broken go.mod of the workspace makes Related fail; the error must reach the caller.
func TestRelatedReturnsErrorForBrokenGoMod(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	root := t.TempDir()
	writeTree(t, root, map[string]string{"go.mod": "this is not a go.mod\n", "s.go": "package s\n"})
	if _, err := Related(context.Background(), root, []string{filepath.Join(root, "s.go")}); err == nil {
		t.Errorf("broken go.mod: Related must return the error, not discard it")
	}
}
