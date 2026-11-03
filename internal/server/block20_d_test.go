package server

import (
	"strings"
	"testing"
)

// BUG-MOVE-003 (review): moving a package directory to lib-x rewrites its importers to lib-x; those rewritten lines are
// not reported as stale mentions of lib, and a Makefile line that names lib-x is not either.
func TestMoveDirectoryPrefixOfNewPathNotStale(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "libpkg/lib.go", "package lib\n\nfunc V() int { return 1 }\n")
	writeFile(t, root, "usepkg/use.go", "package use\n\nimport \"example.com/fx/libpkg\"\n\nfunc U() int { return lib.V() }\n")
	writeFile(t, root, "Makefile", "build:\n\tgo build example.com/fx/libpkg-x\n")

	out := call(t, cs, "move", map[string]any{"file_path": "libpkg", "to_file": "libpkg-x"})
	mustHave(t, "move dir", out, "Applied")
	if strings.Contains(out, "still mention the old import path") {
		t.Fatalf("a rewritten import of libpkg-x must not be reported as a stale mention of libpkg:\n%s", out)
	}
	if !strings.Contains(readFile(t, root, "usepkg/use.go"), `"example.com/fx/libpkg-x"`) {
		t.Fatal("the importer must be rewritten to libpkg-x")
	}
}

// BUG-MOVE-003 (review): the same move, with a Makefile line that still names the old path, reports that line (the
// wiring from the move to the leftover scan), and only that line.
func TestMoveDirectoryReportsStaleMention(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "libpkg/lib.go", "package lib\n\nfunc V() int { return 1 }\n")
	writeFile(t, root, "usepkg/use.go", "package use\n\nimport \"example.com/fx/libpkg\"\n\nfunc U() int { return lib.V() }\n")
	writeFile(t, root, "Makefile", "build:\n\tgo build example.com/fx/libpkg-x\n\tgo run example.com/fx/libpkg\n")

	out := call(t, cs, "move", map[string]any{"file_path": "libpkg", "to_file": "libpkg-x"})
	mustHave(t, "move dir", out, "Applied", "1 place(s) still mention the old import path example.com/fx/libpkg (not rewritten): Makefile:3 (go run example.com/fx/libpkg).")
}
