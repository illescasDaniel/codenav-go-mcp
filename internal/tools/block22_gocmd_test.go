package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// resetGoEnvs forgets the answers of `go env` (and the failures kept for goEnvRetry), so each test starts unknown.
func resetGoEnvs() {
	goEnvs.Lock()
	defer goEnvs.Unlock()
	goEnvs.vals = map[string]string{}
	goEnvs.retry = map[string]time.Time{}
}

// b22Sleep is the sleep command, looked up before a test replaces PATH.
var b22Sleep, _ = exec.LookPath("sleep")

// b22HungGo puts a `go` on PATH that sleeps in a child process holding the output pipe open, so only the timeout and
// WaitDelay return a call to it.
func b22HungGo(t *testing.T) {
	t.Helper()
	if b22Sleep == "" {
		t.Skip("sleep is not on PATH")
	}
	b22Go(t, "#!/bin/sh\n"+b22Sleep+" 30\n")
}

// b22Go puts a `go` script with the given text first on PATH (the directory is replaced on each call).
func b22Go(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// Block 22 item 2: readableRoots runs `go env` under goEnvTimeout too; a hung go command does not hang the read tools,
// and the directories of the go.mod replacements are still answered.
func TestBlock22_ReadableRootsBoundedWhenGoHangs(t *testing.T) {
	b22HungGo(t)
	resetGoEnvs()
	oldTimeout := goEnvTimeout
	goEnvTimeout = 300 * time.Millisecond
	t.Cleanup(func() {
		goEnvTimeout = oldTimeout
		resetGoEnvs()
	})
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/root\n\ngo 1.21\n\nreplace example.com/dep => ./dep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got := readableRoots(root)
	if elapsed := time.Since(start); elapsed > 7*time.Second {
		t.Errorf("readableRoots took %s with a hung go command, want the go env timeout", elapsed)
	}
	if !slices.Contains(got, filepath.Join(root, "dep")) {
		t.Errorf("readableRoots = %v, want the replacement directory", got)
	}
}

// Block 22 item 2: a timed-out `go env GOROOT` is not kept as unknown for the process lifetime. The first call finds no
// Go root (the link is kept); once goEnvRetry has passed, a working go answers and the Go root filters the links.
func TestBlock22_StdlibTopRetriesAfterTimeout(t *testing.T) {
	b22HungGo(t)
	resetGoEnvs()
	oldTimeout, oldRetry := goEnvTimeout, goEnvRetry
	goEnvTimeout, goEnvRetry = 300*time.Millisecond, 0
	t.Cleanup(func() {
		goEnvTimeout, goEnvRetry = oldTimeout, oldRetry
		resetGoEnvs()
	})
	goRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(goRoot, "src", "fmt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !stdlibTop("nosuchpackage") {
		t.Fatal("hung go: stdlibTop = false, want true (the Go root is unknown)")
	}
	b22Go(t, "#!/bin/sh\necho "+goRoot+"\n")
	if stdlibTop("nosuchpackage") {
		t.Error("after the timeout, the Go root is still unknown: stdlibTop(nosuchpackage) = true, want false (not a src directory)")
	}
}

// Block 22 item 2: a Go root that was found is kept; a later hung go command does not make the lookup run again.
func TestBlock22_StdlibTopKeepsFoundGoRoot(t *testing.T) {
	goRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(goRoot, "src", "fmt"), 0o755); err != nil {
		t.Fatal(err)
	}
	b22Go(t, "#!/bin/sh\necho "+goRoot+"\n")
	resetGoEnvs()
	oldRetry := goEnvRetry
	goEnvRetry = 0
	t.Cleanup(func() {
		goEnvRetry = oldRetry
		resetGoEnvs()
	})
	if stdlibTop("nosuchpackage") {
		t.Fatal("known Go root: stdlibTop(nosuchpackage) = true, want false")
	}
	b22HungGo(t)
	start := time.Now()
	if !stdlibTop("fmt") || stdlibTop("nosuchpackage") {
		t.Error("the found Go root was not kept")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("found Go root was looked up again: took %s", elapsed)
	}
}
