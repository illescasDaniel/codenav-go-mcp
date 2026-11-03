package gomod

import (
	"context"
	"os/exec"
	"testing"
)

// SHAPE-007: with GO111MODULE=off the health says the workspace is in GOPATH mode and how to fix it, instead of
// reporting the go command's "list -m cannot be used" refusal as a broken configuration.
func TestHealthGOPATHModeNamesTheFix(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	root := t.TempDir()
	writeTree(t, root, map[string]string{"go.mod": "module example.com/legacy\n\ngo 1.21\n"})
	t.Setenv("GO111MODULE", "on")
	if p := Health(context.Background(), root); p != "" {
		t.Fatalf("a module with GO111MODULE on is healthy, got %q", p)
	}
	// The health answer is cached per fingerprint: switching the mode must not return the cached "healthy".
	t.Setenv("GO111MODULE", "off")
	const want = "GOPATH mode is not supported by gopls; add a go.mod"
	if p := Health(context.Background(), root); p != want {
		t.Errorf("GO111MODULE=off: want %q, got %q", want, p)
	}
}
