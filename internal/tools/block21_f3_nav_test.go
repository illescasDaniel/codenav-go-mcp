package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

// b21fNested writes a go.mod for each of the named nested modules below root (directories relative to root).
func b21fNested(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		p := filepath.Join(root, d)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		mod := fmt.Sprintf("module example.com/root/%s\n\ngo 1.21\n", filepath.ToSlash(d))
		if err := os.WriteFile(filepath.Join(p, "go.mod"), []byte(mod), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// BUG-20N1 follow-up (a): a go.work that starts to cover a nested module is seen at once: the configuration
// fingerprint changes, so the cached scan is not used.
func TestBlock21F3_NestedScanSeesChangedGoWork(t *testing.T) {
	t.Setenv("GOWORK", "")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/root\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b21fNested(t, root, "sub")
	if got := nestedScan(root); len(got.mods) != 1 {
		t.Fatalf("first scan: %+v, want the uncovered sub module", got)
	}
	work := "go 1.21\n\nuse (\n\t.\n\t./sub\n)\n"
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte(work), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := nestedScan(root); len(got.mods) != 0 {
		t.Fatalf("after go.work covers sub: %+v, want no uncovered module (cache not invalidated)", got)
	}
}

// BUG-20N1 follow-up (a): a truncated scan is not kept for the full TTL. With four nested modules the scan stops at
// nestedLimit (3); after the short TTL the rescan sees the module that was out of reach.
func TestBlock21F3_NestedScanTruncatedIsRescanned(t *testing.T) {
	t.Setenv("GOWORK", "")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/root\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b21fNested(t, root, "a", "b", "c", "d")
	oldTTL := nestedTruncatedTTL
	nestedTruncatedTTL = 20 * time.Millisecond
	t.Cleanup(func() { nestedTruncatedTTL = oldTTL })

	first := nestedScan(root)
	if !first.truncated || len(first.mods) != nestedLimit {
		t.Fatalf("first scan: %+v, want %d modules and truncated", first, nestedLimit)
	}
	gone := first.mods[0].Dir
	if err := os.Remove(filepath.Join(gone, "go.mod")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	second := nestedScan(root)
	for _, m := range second.mods {
		if m.Dir == gone {
			t.Fatalf("removed module %s still listed: the truncated scan was cached past its TTL", gone)
		}
	}
	if len(second.mods) != nestedLimit {
		t.Errorf("rescan: %+v, want the module that was out of reach to be listed", second)
	}
}

// BUG-20N1 (c): a file over 2 MB is not parsed, and the answer says so; a small broken file still gets its syntax note.
func TestBlock21F3_OwnSyntaxNoteSizeCap(t *testing.T) {
	root := t.TempDir()
	w := &Workspace{Root: root}

	big := filepath.Join(root, "big", "big.go")
	if err := os.MkdirAll(filepath.Dir(big), 0o755); err != nil {
		t.Fatal(err)
	}
	padding := strings.Repeat("// padding padding padding padding padding\n", 3<<20/42+1)
	if err := os.WriteFile(big, []byte("package big\n\nfunc (\n"+padding), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(big); err != nil || st.Size() <= maxSyntaxCheckBytes {
		t.Fatalf("fixture is not over the cap: %v", err)
	}
	want := "big/big.go: not checked for syntax errors (file is over 2 MB)"
	if got := ownSyntaxNote(w, big); got != want {
		t.Errorf("over-size note = %q, want %q", got, want)
	}

	small := filepath.Join(root, "small.go")
	if err := os.WriteFile(small, []byte("package small\n\nfunc (\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ownSyntaxNote(w, small); !strings.HasPrefix(got, "small.go:3:") || !strings.Contains(got, "syntax error") {
		t.Errorf("small broken file note = %q, want the syntax error", got)
	}

	valid := filepath.Join(root, "valid.go")
	if err := os.WriteFile(valid, []byte("package valid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ownSyntaxNote(w, valid); got != "" {
		t.Errorf("valid file note = %q, want none", got)
	}
}

// BUG-20N1 follow-up (b): gopls' "no views" without a broken configuration is worded as a missing workspace.
func TestBlock21F3_FriendlyNavErrNoViews(t *testing.T) {
	err := friendlyNavErr(&lsp.RequestError{Method: "textDocument/documentSymbol", Message: "no views"}, "x/x.go", 0, 0)
	want := "the language server has no workspace for x/x.go (no go.mod or go.work covers it)"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if toolerr.IsNotFound(err) {
		t.Error("a missing workspace is a refusal, not a not-found answer")
	}
}

// navErr keeps the wording for a healthy configuration, and the broken-configuration wording is unchanged.
func TestBlock21F3_NavErrNoViewsHealthyConfig(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go command is not on PATH")
	}
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "off")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/healthy\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := &Workspace{Root: root}
	err := w.navErr(context.Background(), &lsp.RequestError{Message: "no views"}, "x/x.go", 1, 1)
	if err == nil || !strings.Contains(err.Error(), "the language server has no workspace for x/x.go") {
		t.Fatalf("navErr = %v, want the missing-workspace wording", err)
	}
}

// BUG-20N1 follow-up (d): a hung `go env GOROOT` does not hang the hover that asks for a pkg.go.dev link. The fake go
// script sleeps in a child process that keeps the output pipe open, so only the timeout and WaitDelay return the call.
func TestBlock21F3_StdlibTopBoundedWhenGoHangs(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep is not on PATH")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" + sleep + " 30\n"
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	oldTimeout := goEnvTimeout
	goEnvTimeout = 300 * time.Millisecond
	resetGoEnvs()
	t.Cleanup(func() {
		goEnvTimeout = oldTimeout
		resetGoEnvs()
	})

	start := time.Now()
	// example.com is not a directory of any Go root: with the root unknown the link is kept (true).
	if got := stdlibTop("example.com"); !got {
		t.Error("stdlibTop with an unknown Go root = false, want true (keeps the link)")
	}
	if elapsed := time.Since(start); elapsed > 7*time.Second {
		t.Errorf("stdlibTop took %s with a hung go command", elapsed)
	}
}
