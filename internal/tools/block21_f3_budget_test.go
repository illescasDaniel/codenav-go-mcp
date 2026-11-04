package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// The causes of one diagnostics answer share one budget. With a hung go command, three broken imports stay near the
// budget instead of waiting for each lookup in full (10 s each, without the shared budget), and every import keeps its
// generic wording, since no cause is known.
func TestBlock21F3_BrokenImportCausesShareOneBudget(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep is not on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte("#!/bin/sh\n"+sleep+" 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	oldBudget := causeBudget
	causeBudget = 300 * time.Millisecond
	t.Cleanup(func() { causeBudget = oldBudget })

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/budget\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	items := []lsp.Diagnostic{
		{Message: `could not import a.example/one (no required module provides package "a.example/one")`},
		{Message: `could not import b.example/two (no required module provides package "b.example/two")`},
		{Message: `could not import c.example/three (no required module provides package "c.example/three")`},
	}
	start := time.Now()
	got := explainBrokenImports(context.Background(), root, filepath.Join(root, "x", "x.go"), items)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("three lookups with a hung go command took %s, want about the shared budget", elapsed)
	}
	for i := range got {
		if got[i].Message != items[i].Message {
			t.Errorf("message %d changed without a cause: %q", i, got[i].Message)
		}
	}
}
