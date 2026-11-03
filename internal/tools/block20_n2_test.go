package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/notices"
)

// fakeHost is a Host whose only answer is the explanation of the workspace choice (a fake manager).
type fakeHost struct{ explain string }

func (f fakeHost) Acquire(context.Context, *mcp.ServerSession) (*Workspace, error) {
	return nil, nil
}
func (f fakeHost) Notes() *notices.Board        { return nil }
func (f fakeHost) Explain(source string) string { return f.explain }

// BUG-ENV-007 and BUG-LEAK-005: workspace() reports the read-only mode, build flags, gopls, the Go version and the
// go.work in use with its GOWORK value, not only the root and the reason.
func TestBlock20NWorkspaceReportsEffectiveSettings(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{
		"go.mod":  "module example.com/n2\n\ngo 1.21\n",
		"go.work": "go 1.21\n\nuse .\n",
		"a/a.go":  "package a\n",
	})
	t.Setenv("GOWORK", "")
	t.Setenv("GOFLAGS", "")
	t.Setenv("CODENAV_GO_MCP_BUILD_FLAGS", "-tags=n2")
	t.Setenv("CODENAV_GO_MCP_READ_ONLY", "1")
	w := &Workspace{Root: root, Source: "env", Journal: nil}
	out := workspaceReport(context.Background(), fakeHost{explain: "pinned by $CODENAV_GO_MCP_WORKSPACE"}, w)
	for _, want := range []string{
		root,
		"read-only: yes (CODENAV_GO_MCP_READ_ONLY)",
		"build flags (CODENAV_GO_MCP_BUILD_FLAGS): -tags=n2",
		"gopls: ",
		"go: ",
		"GOWORK env: unset",
		"go.work in use: " + filepath.Join(root, "go.work"),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("workspace() lacks %q:\n%s", want, out)
		}
	}

	// An explicit GOWORK value is the one reported, and GOWORK=off says so.
	t.Setenv("GOWORK", "off")
	out = workspaceReport(context.Background(), fakeHost{}, w)
	if !strings.Contains(out, "GOWORK env: off") || !strings.Contains(out, "go.work in use: off") {
		t.Errorf("GOWORK=off not reported:\n%s", out)
	}
}

// BUG-LEAK-005 (the no-read-only half): with the read-only variable unset, the report says "no".
func TestBlock20NWorkspaceReportsWritableServer(t *testing.T) {
	t.Setenv("CODENAV_GO_MCP_READ_ONLY", "")
	out := workspaceReport(context.Background(), fakeHost{}, &Workspace{Root: t.TempDir(), Journal: nil})
	if !strings.Contains(out, "read-only: no") {
		t.Errorf("writable server:\n%s", out)
	}
}

// BUG-NAVN-012: the implementers list drops unexported foreign types, including a method's receiver type, and splits
// the workspace's own types from the standard library's and the dependencies'.
func TestBlock20NImplementersGroupedAndPrivateForeignDropped(t *testing.T) {
	root := t.TempDir()
	goroot := t.TempDir()
	modcache := t.TempDir()
	files := map[string]string{
		filepath.Join(root, "p", "sink.go"):             "package p\n\ntype FileSink struct{}\n\nfunc (FileSink) Write(b []byte) (int, error) { return 0, nil }\n",
		filepath.Join(goroot, "src", "fmt", "print.go"): "package fmt\n\ntype pp struct{}\n\nfunc (p *pp) Write(b []byte) (int, error) { return 0, nil }\n",
		filepath.Join(goroot, "src", "io", "io.go"):     "package io\n\ntype Writer interface {\n\tWrite(p []byte) (n int, err error)\n}\n",
		filepath.Join(modcache, "x@v1", "a", "a.go"):    "package a\n\ntype Reader interface{ Read() }\n\ntype buf struct{}\n\nfunc (buf) Read() {}\n",
	}
	for p, text := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	locs := []lsp.Location{
		testLoc(filepath.Join(root, "p", "sink.go"), files[filepath.Join(root, "p", "sink.go")], "FileSink struct"),
		testLoc(filepath.Join(goroot, "src", "fmt", "print.go"), files[filepath.Join(goroot, "src", "fmt", "print.go")], "Write(b"),
		testLoc(filepath.Join(goroot, "src", "io", "io.go"), files[filepath.Join(goroot, "src", "io", "io.go")], "Writer interface"),
		testLoc(filepath.Join(modcache, "x@v1", "a", "a.go"), files[filepath.Join(modcache, "x@v1", "a", "a.go")], "Reader interface"),
		testLoc(filepath.Join(modcache, "x@v1", "a", "a.go"), files[filepath.Join(modcache, "x@v1", "a", "a.go")], "Read() {}"),
	}
	dirs := toolchainDirs{GOROOT: goroot, GOMODCACHE: modcache}
	kept := dropForeignUnexported(locs, root)
	if len(kept) != 3 {
		t.Fatalf("kept %d locations, want 3 (the fmt method and the unexported dependency method are dropped)", len(kept))
	}
	out := implementerGroups(kept, root, dirs)
	for _, want := range []string{"Types (1): FileSink", "Standard library (1): io.Writer", "Dependencies (1): a.Reader"} {
		if !strings.Contains(out, want) {
			t.Errorf("grouped implementers lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "pp") || strings.Contains(out, "buf") {
		t.Errorf("unexported foreign type listed:\n%s", out)
	}
}

// testLoc is the location of the first occurrence of needle in text (a 0-based line and column).
func testLoc(path, text, needle string) lsp.Location {
	i := strings.Index(text, needle)
	before := text[:i]
	line := strings.Count(before, "\n")
	col := len(before) - (strings.LastIndex(before, "\n") + 1)
	var l lsp.Location
	l.URI = "file://" + filepath.ToSlash(path)
	l.Range.Start = lsp.Position{Line: line, Character: col}
	return l
}

// BUG-NAVP-003: a reference that sits in the Go root or the module cache is not listed, and the number is said.
func TestBlock20NReferencesDropDependencyCallSites(t *testing.T) {
	root := t.TempDir()
	goroot := t.TempDir()
	modcache := t.TempDir()
	dirs := toolchainDirs{GOROOT: goroot, GOMODCACHE: modcache}
	locs := []lsp.Location{
		{URI: "file://" + filepath.ToSlash(filepath.Join(root, "use.go"))},
		{URI: "file://" + filepath.ToSlash(filepath.Join(modcache, "github.com", "x@v1", "assert", "a.go"))},
		{URI: "file://" + filepath.ToSlash(filepath.Join(modcache, "github.com", "x@v1", "assert", "b.go"))},
		{URI: "file://" + filepath.ToSlash(filepath.Join(goroot, "src", "fmt", "print.go"))},
	}
	kept, dropped := dropDependencyLocs(locs, root, dirs)
	if dropped != 3 || len(kept) != 1 || lsp.URIToPath(kept[0].URI) != filepath.Join(root, "use.go") {
		t.Errorf("kept %d, dropped %d; want the workspace reference only and 3 dropped", len(kept), dropped)
	}
	// A workspace file under a directory that is also a dependency directory stays.
	inside := filepath.Join(modcache, "ws")
	kept, dropped = dropDependencyLocs([]lsp.Location{{URI: "file://" + filepath.ToSlash(filepath.Join(inside, "x.go"))}}, inside, dirs)
	if dropped != 0 || len(kept) != 1 {
		t.Errorf("a workspace file under the module cache was dropped: kept %d, dropped %d", len(kept), dropped)
	}
	if plural(1) != "" || plural(2) != "s" {
		t.Errorf("plural wrong")
	}
}

// BUG-NAVN-013: the callers answer knows whether the symbol is an interface method.
func TestBlock20NInterfaceMethodDetected(t *testing.T) {
	src := "package p\n\ntype Sink interface{ Write(b []byte) }\n\ntype FileSink struct{}\n\nfunc (FileSink) Write(b []byte) {}\n"
	inIface := strings.Index(src, "Write(b []byte) }")
	inStruct := strings.Index(src, "Write(b []byte) {}")
	if !interfaceTypeAt("p.go", src, inIface) {
		t.Errorf("interface method not recognised")
	}
	if interfaceTypeAt("p.go", src, inStruct) {
		t.Errorf("concrete method taken for an interface method")
	}
}

// BUG-NAVN-011: a nested module the workspace does not cover that mentions the queried name gets a note.
func TestBlock20NNestedModuleImplementersNoted(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{
		"go.mod":       "module example.com/nested\n\ngo 1.21\n",
		"p/iface.go":   "package p\n\ntype Iface interface{ Do() }\n",
		"sub/go.mod":   "module example.com/nested/sub\n\ngo 1.21\n",
		"sub/impl.go":  "package sub\n\nimport \"example.com/nested/p\"\n\ntype Impl struct{}\n\nfunc (Impl) Do() {}\n\nvar _ p.Iface = Impl{}\n",
		"other/go.mod": "module example.com/nested/other\n\ngo 1.21\n",
		"other/o.go":   "package other\n\nfunc O() {}\n",
	})
	w := &Workspace{Root: root}
	note := nestedImplementersNote(w, "Iface")
	if !strings.Contains(note, "nested module example.com/nested/sub (sub/go.mod)") {
		t.Errorf("no note for the nested module that mentions Iface:\n%q", note)
	}
	if strings.Contains(note, "other") {
		t.Errorf("a module that does not mention Iface is noted:\n%s", note)
	}
	if got := nestedImplementersNote(w, "NoSuchName"); got != "" {
		t.Errorf("note for a name no nested module mentions:\n%s", got)
	}
}

// BUG-NAVN-011 (negative half of N1 test (a)): a healthy go.mod keeps the build-constraints wording for a
// "no package metadata" message.
func TestBlock20NNavErrHealthyGoModKeepsBuildConstraints(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{
		"go.mod":   "module example.com/n2ok\n\ngo 1.21\n",
		"m1/m1.go": "package m1\n\nfunc M1() int { return 1 }\n",
	})
	w := &Workspace{Root: root}
	msg := "no package metadata for file://" + filepath.Join(root, "m1", "m1.go")
	err := w.navErr(context.Background(), &lsp.RequestError{Method: "textDocument/hover", Message: msg}, "m1/m1.go", 3, 6)
	if err == nil || !strings.Contains(err.Error(), "build constraints") || strings.Contains(err.Error(), "configuration is broken") {
		t.Errorf("healthy go.mod, no package metadata: %v", err)
	}
}

// Test (b) of batch N1: an existing README.md passed to hover or outline is refused as not a .go file.
func TestBlock20NExistingReadmeIsNotGoFile(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{"README.md": "# readme\n"})
	w := &Workspace{Root: root}
	err := checkGoFile(w, "README.md")
	if err == nil || !strings.Contains(err.Error(), "not a .go file") {
		t.Errorf("existing README.md: %v", err)
	}
}
