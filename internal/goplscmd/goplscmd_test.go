package goplscmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// script writes an executable file at dir/name and returns its path.
func script(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// isolate clears the variables the search reads and points PATH at an empty directory, so only the
// test's own binaries can be found.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("CODENAV_GO_MCP_GOPLS", "")
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")
	t.Setenv("PATH", t.TempDir())
}

// An empty GOBIN is no directory, and a multi-entry GOPATH is searched in order.
func TestEmptyGobinAndMultiEntryGopath(t *testing.T) {
	isolate(t)
	base := t.TempDir()
	want := script(t, filepath.Join(base, "two", "bin"), "gopls")
	t.Setenv("GOPATH", filepath.Join(base, "one")+string(os.PathListSeparator)+filepath.Join(base, "two"))
	cmd, warn, err := Command()
	if err != nil || warn != "" || len(cmd) != 1 || cmd[0] != want {
		t.Fatalf("Command() = %v, %q, %v; want %s", cmd, warn, err, want)
	}
}

// An unusable pin is an error that names the variable; it is not replaced by a search elsewhere.
func TestUnusablePinIsAnError(t *testing.T) {
	isolate(t)
	base := t.TempDir()
	script(t, filepath.Join(base, "bin"), "gopls")
	t.Setenv("GOPATH", base)
	t.Setenv("CODENAV_GO_MCP_GOPLS", filepath.Join(base, "missing", "gopls"))
	if _, _, err := Command(); err == nil || !strings.Contains(err.Error(), "CODENAV_GO_MCP_GOPLS") {
		t.Fatalf("unusable pin: %v", err)
	}
}

// A pin with arguments is split on whitespace; the arguments travel with the command.
func TestPinWithArguments(t *testing.T) {
	isolate(t)
	bin := script(t, t.TempDir(), "gopls")
	t.Setenv("CODENAV_GO_MCP_GOPLS", bin+" -remote=auto")
	cmd, _, err := Command()
	if err != nil || len(cmd) != 2 || cmd[0] != bin || cmd[1] != "-remote=auto" {
		t.Fatalf("Command() = %v, %v", cmd, err)
	}
}

// With no gopls and no go command on PATH there is no command, and the answer says so.
func TestNoGoplsNoGo(t *testing.T) {
	isolate(t)
	t.Setenv("GOPATH", t.TempDir())
	if _, _, err := Command(); err == nil || !strings.Contains(err.Error(), "gopls not found") {
		t.Fatalf("no gopls: %v", err)
	}
}

// With no gopls but a go command, the fallback is named by a warning.
func TestGoRunFallbackWarns(t *testing.T) {
	isolate(t)
	t.Setenv("GOPATH", t.TempDir())
	dir := t.TempDir()
	script(t, dir, "go")
	t.Setenv("PATH", dir)
	cmd, warn, err := Command()
	if err != nil || warn == "" || len(cmd) != 3 || cmd[1] != "run" {
		t.Fatalf("fallback: %v, %q, %v", cmd, warn, err)
	}
}
