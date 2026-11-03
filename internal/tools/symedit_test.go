package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
)

// locateText is the located form of the declaration called name in text, as replaceSymbol sees it.
func locateText(t *testing.T, path, text, name string) *located {
	t.Helper()
	d, _, err := gosrc.FindDeclByName(path, text, name)
	if err != nil {
		t.Fatal(err)
	}
	return &located{path: path, text: text, orig: text, decl: d}
}

// ESYM-001: a replacement that changes what the constants after it inherit is refused, and one that
// keeps the inherited value is allowed.
func TestConstRepeatsRefusal(t *testing.T) {
	const text = "package p\n\nconst (\n\tRed = iota\n\tGreen\n\tBlue\n)\n"
	red := locateText(t, "p.go", text, "Red")
	if err := constRepeatsRefusal(red, "Red = 10"); err == nil || !strings.Contains(err.Error(), "Green, Blue") || !strings.Contains(err.Error(), `replace Green with "Green = iota"`) {
		t.Errorf("replace Red = 10: want a refusal naming Green, Blue and the way forward, got %v", err)
	}
	if err := constRepeatsRefusal(red, "Red = iota"); err != nil {
		t.Errorf("unchanged value: %v", err)
	}
	green := locateText(t, "p.go", text, "Green")
	if err := constRepeatsRefusal(green, "Green = iota"); err != nil {
		t.Errorf("Green = iota keeps the value Blue inherits: %v", err)
	}
	if err := constRepeatsRefusal(green, "Green = 7"); err == nil || !strings.Contains(err.Error(), "Blue") {
		t.Errorf("replace Green = 7: want a refusal naming Blue, got %v", err)
	}
	// Implicit repeat of a plain expression, not iota.
	const strs = "package p\n\nconst (\n\tX = \"a\"\n\tY\n)\n"
	if err := constRepeatsRefusal(locateText(t, "p.go", strs, "X"), `X = "b"`); err == nil || !strings.Contains(err.Error(), "Y") {
		t.Errorf("implicit repeat: want a refusal naming Y, got %v", err)
	}
	// No later spec depends on it: nothing to check.
	const explicit = "package p\n\nconst (\n\tA = 1\n\tB = 2\n)\n"
	if err := constRepeatsRefusal(locateText(t, "p.go", explicit, "A"), "A = 5"); err != nil {
		t.Errorf("explicit values: %v", err)
	}
}

// ESYM-009: the //go: and //export lines of the old doc block that the replacement does not repeat.
func TestLostDirectives(t *testing.T) {
	const text = "package p\n\n// Parse parses s.\n//\n//go:noinline\n//export Parse\nfunc Parse() {}\n"
	l := locateText(t, "p.go", text, "Parse")
	if got := lostDirectives(l, "func Parse() {}"); strings.Join(got, "|") != "//go:noinline|//export Parse" {
		t.Errorf("no directive repeated: %q", got)
	}
	if got := lostDirectives(l, "//go:noinline\nfunc Parse() {}"); strings.Join(got, "|") != "//export Parse" {
		t.Errorf("one directive repeated: %q", got)
	}
	if got := lostDirectives(l, "// Parse is new.\n//go:noinline\n//export Parse\nfunc Parse() {}"); len(got) != 0 {
		t.Errorf("all repeated: %q", got)
	}
	// A comment that is not a directive, and a directive below the declaration, are not doc directives.
	const plain = "package p\n\n// Parse parses s.\n// go:noinline is prose here.\nfunc Parse() {}\n"
	if got := lostDirectives(locateText(t, "p.go", plain, "Parse"), "func Parse() {}"); len(got) != 0 {
		t.Errorf("prose is not a directive: %q", got)
	}
}

// ESYM-010: the exported-name note and each condition that skips it.
func TestExportNoteAndSkips(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/x\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(root, "lib", "lib.go")
	const libText = "package lib\n\nfunc Parse() {}\n"
	l := locateText(t, lib, libText, "Parse")
	unused := "Parse was exported from example.com/x/lib; nothing in this workspace uses it, but code outside the workspace that imports lib no longer builds."
	if got := exportNote(l, true); got != unused {
		t.Errorf("unused exported func:\n got %q\nwant %q", got, unused)
	}
	if got := exportNote(l, false); got != "Parse was exported from example.com/x/lib; code outside the workspace that imports lib no longer builds." {
		t.Errorf("used exported func: %q", got)
	}
	for _, c := range []struct{ name, path, text, key string }{
		{"package main", filepath.Join(root, "app", "main.go"), "package main\n\nfunc Parse() {}\n", "Parse"},
		{"internal package", filepath.Join(root, "internal", "q", "q.go"), "package q\n\nfunc Parse() {}\n", "Parse"},
		{"_test.go file", filepath.Join(root, "lib", "lib_test.go"), "package lib\n\nfunc Parse() {}\n", "Parse"},
		{"unexported name", lib, "package lib\n\nfunc parse() {}\n", "parse"},
		{"method on unexported type", lib, "package lib\n\ntype t struct{}\n\nfunc (t) M() {}\n", "t.M"},
	} {
		if got := exportNote(locateText(t, c.path, c.text, c.key), true); got != "" {
			t.Errorf("%s: want no note, got %q", c.name, got)
		}
	}
	// A method on an exported type is exported, and its note names the receiver.
	if got := exportNote(locateText(t, lib, "package lib\n\ntype T struct{}\n\nfunc (T) M() {}\n", "T.M"), true); !strings.HasPrefix(got, "T.M was exported from example.com/x/lib;") {
		t.Errorf("method on exported type: %q", got)
	}
}
