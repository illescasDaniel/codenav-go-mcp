//go:build unix

package tools

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// BUG-MOVE-003 (review): the leftover scan never opens a FIFO and does not follow a symlink, so a FIFO in the tree
// cannot hang the move and a link is not reported as a file of its own.
func TestStaleImportNotesSkipFIFOAndSymlink(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{
		"go.mod":   b20dMod,
		"Makefile": "build:\n\tgo build example.com/mv/lib\n",
		"real.txt": "example.com/mv/lib\n",
	})
	for _, name := range []string{"pipe", "notes.md"} {
		if err := syscall.Mkfifo(filepath.Join(root, name), 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "real.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	ch := make(chan string, 1)
	go func() {
		ch <- strings.Join(staleImportNotes(&Workspace{Root: root}, "example.com/mv/lib", "", nil, nil), "\n")
	}()
	var got string
	select {
	case got = <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("the leftover scan blocked on a FIFO in the tree")
	}
	for _, want := range []string{"Makefile:2", "real.txt:1"} {
		if !strings.Contains(got, want) {
			t.Errorf("notes must contain %q, got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "link.txt") {
		t.Errorf("a symlink must not be reported, got:\n%s", got)
	}
}
