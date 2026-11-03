package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeRoots stands in for a client session: it counts roots/list calls.
type fakeRoots struct {
	calls int
	err   error
}

func (f *fakeRoots) InitializeParams() *mcp.InitializeParams {
	return &mcp.InitializeParams{Capabilities: &mcp.ClientCapabilities{RootsV2: &mcp.RootCapabilities{}}}
}

func (f *fakeRoots) ListRoots(context.Context, *mcp.ListRootsParams) (*mcp.ListRootsResult, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &mcp.ListRootsResult{}, nil
}

// SHAPE-004: a pinned workspace ignores the client's roots, so roots/list is never sent.
func TestPinnedWorkspaceNeverAsksForRoots(t *testing.T) {
	t.Setenv(EnvWorkspace, t.TempDir())
	m := NewManager()
	fake := &fakeRoots{}
	if got := m.clientRoots(context.Background(), fake); got != nil {
		t.Fatalf("pinned: roots = %v, want nil", got)
	}
	if fake.calls != 0 {
		t.Fatalf("pinned workspace asked for roots %d time(s), want 0", fake.calls)
	}
}

// SHAPE-004: an unpinned session whose roots/list failed is not asked again.
func TestFailedRootsListNotRepeatedForSession(t *testing.T) {
	t.Setenv(EnvWorkspace, "")
	m := NewManager()
	fake := &fakeRoots{err: errors.New("no roots")}
	ctx := context.Background()
	m.clientRoots(ctx, fake)
	m.clientRoots(ctx, fake)
	m.clientRoots(ctx, fake)
	if fake.calls != 1 {
		t.Fatalf("roots/list asked %d times for one failing session, want 1", fake.calls)
	}
	other := &fakeRoots{}
	m.clientRoots(ctx, other)
	if other.calls != 1 {
		t.Fatalf("a new session was not asked for roots (calls=%d)", other.calls)
	}
}

// A cancelled tool call says nothing about the client: its failed roots/list must not disable roots.
func TestCancelledCallDoesNotDisableRoots(t *testing.T) {
	t.Setenv(EnvWorkspace, "")
	m := NewManager()
	fake := &fakeRoots{err: errors.New("context canceled")}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	m.clientRoots(cancelled, fake)
	m.clientRoots(context.Background(), fake)
	if fake.calls != 2 {
		t.Fatalf("roots/list asked %d times, want 2: a cancelled call disabled roots", fake.calls)
	}
}

// A failed roots/list is retried once the back-off has passed.
func TestRootsRetriedAfterBackoff(t *testing.T) {
	old := rootsRetryAfter
	rootsRetryAfter = 0
	t.Cleanup(func() { rootsRetryAfter = old })
	t.Setenv(EnvWorkspace, "")
	m := NewManager()
	fake := &fakeRoots{err: errors.New("slow")}
	m.clientRoots(context.Background(), fake)
	m.clientRoots(context.Background(), fake)
	if fake.calls != 2 {
		t.Fatalf("roots/list asked %d times, want 2 after back-off", fake.calls)
	}
}

// notifications/roots/list_changed clears a remembered failure, so the client is asked again at once.
func TestRootsListChangedClearsFailure(t *testing.T) {
	t.Setenv(EnvWorkspace, "")
	m := NewManager()
	fake := &fakeRoots{err: errors.New("no roots")}
	ctx := context.Background()
	m.clientRoots(ctx, fake)
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) { return nil, nil }
	if _, err := m.rootsChanged(next)(ctx, "notifications/roots/list_changed", nil); err != nil {
		t.Fatal(err)
	}
	m.clientRoots(ctx, fake)
	if fake.calls != 2 {
		t.Fatalf("roots/list asked %d times, want 2 after list_changed", fake.calls)
	}
}

// ENV-003: a pinned workspace that is missing or a file is named as such, with the variable.
func TestPinnedWorkspaceMustBeADirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(file, []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, pin := range []string{filepath.Join(dir, "nope"), file} {
		t.Setenv(EnvWorkspace, pin)
		m := NewManager()
		_, err := m.Acquire(context.Background(), nil)
		want := "$" + EnvWorkspace + "=" + pin + " is not a directory"
		if err == nil || err.Error() != want {
			t.Errorf("pin %s: err = %v, want %q", pin, err, want)
		}
	}
}

// ENV-003: a missing CODENAV_GO_MCP_GOPLS names the path and the variable, not the workspace root.
func TestMissingGoplsPinNamesPath(t *testing.T) {
	t.Setenv(EnvGopls, "/nonexistent/dir/gopls")
	_, _, err := GoplsCommand()
	want := "gopls not found at /nonexistent/dir/gopls ($" + EnvGopls + ")"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// ENV-011: an executable whose path has a space is one argument, not split at the space.
func TestGoplsPinWithSpaceInPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dir with space")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "gopls")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvGopls, bin)
	cmd, _, err := GoplsCommand()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(cmd) != 1 || cmd[0] != bin {
		t.Fatalf("cmd = %q, want [%q]", cmd, bin)
	}
}

// ENV-011: a value that is not an existing file is still split on whitespace ("sh -c" style).
func TestGoplsPinSplitsWhenNotAFile(t *testing.T) {
	t.Setenv(EnvGopls, "sh -c")
	cmd, _, err := GoplsCommand()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(cmd) != 2 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("cmd = %q, want [sh -c]", cmd)
	}
}

// ENV-004: a bare "~" pin expands to the home directory, as "~/" does.
func TestBareTildeWorkspaceExpandsToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvWorkspace, "~")
	m := NewManager()
	want, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Selector().Base; got != want {
		t.Fatalf("base = %q, want home %q", got, want)
	}
}

// ENV-008: GOCACHE=off is announced at startup, since references and cgo hover come back incomplete.
func TestGocacheOffNotice(t *testing.T) {
	t.Setenv("GOCACHE", "off")
	_, m := New("test")
	out := m.Notes().Annotate("")
	if !strings.Contains(out, "GOCACHE=off: references and cgo hover can be incomplete") {
		t.Fatalf("no GOCACHE notice in %q", out)
	}
}

func TestNoGocacheNoticeByDefault(t *testing.T) {
	t.Setenv("GOCACHE", "")
	_, m := New("test")
	if out := m.Notes().Annotate(""); strings.Contains(out, "GOCACHE") {
		t.Fatalf("unexpected GOCACHE notice: %q", out)
	}
}

// ENV-010: a root with neither go.mod nor go.work gets the not-a-Go-project notice.
func TestNotAGoProjectNotice(t *testing.T) {
	const want = "not a Go project (no go.mod or go.work); symbol answers will be empty"
	m := NewManager()
	m.noteRoot(t.TempDir())
	if out := m.Notes().Annotate(""); !strings.Contains(out, want) {
		t.Fatalf("empty dir: no notice in %q", out)
	}

	withMod := t.TempDir()
	if err := os.WriteFile(filepath.Join(withMod, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m2 := NewManager()
	m2.noteRoot(withMod)
	if out := m2.Notes().Annotate(""); strings.Contains(out, "not a Go project") {
		t.Fatalf("go.mod root: unexpected notice in %q", out)
	}
}
