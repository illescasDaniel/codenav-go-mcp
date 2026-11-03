package server

import (
	"strings"
	"testing"
)

// BUG-ESYM-001: replacing or deleting a constant that later constants inherit from is refused, with
// the way forward; the explicit-value route is allowed and keeps every value.
func TestEditSymbolConstInheritedValues(t *testing.T) {
	cs, root := session(t, nil)
	const iota = "package esym\n\nconst (\n\tRed = iota\n\tGreen\n\tBlue\n)\n"
	writeFile(t, root, "esym/c.go", iota)
	out := call(t, cs, "edit_symbol", map[string]any{"action": "replace", "name": "Red", "file_path": "esym/c.go", "source": "Red = 10"})
	mustHave(t, "replace Red over iota", out, "Green, Blue", "take their value from it", `replace Green with "Green = iota"`)
	if got := readFile(t, root, "esym/c.go"); got != iota {
		t.Fatalf("refused replace wrote the file:\n%s", got)
	}
	// The way forward: give Green its value explicitly (the same value), then Red.
	out = call(t, cs, "edit_symbol", map[string]any{"action": "replace", "name": "Green", "file_path": "esym/c.go", "source": "Green = iota"})
	mustHave(t, "replace Green with the same value", out, "Applied")
	out = call(t, cs, "edit_symbol", map[string]any{"action": "replace", "name": "Red", "file_path": "esym/c.go", "source": "Red = 10"})
	mustHave(t, "replace Red once Green is explicit", out, "Applied")
	// Red is 10, Green is iota (1), Blue inherits Green's iota (2): no value moved.
	if got, want := readFile(t, root, "esym/c.go"), "package esym\n\nconst (\n\tRed   = 10\n\tGreen = iota\n\tBlue\n)\n"; got != want {
		t.Fatalf("explicit route:\n got %q\nwant %q", got, want)
	}

	// Implicit repeat of a plain expression (no iota): the same refusal.
	writeFile(t, root, "esym/s.go", "package esym\n\nconst (\n\tX = \"a\"\n\tY\n)\n")
	out = call(t, cs, "edit_symbol", map[string]any{"action": "replace", "name": "X", "file_path": "esym/s.go", "source": `X = "b"`})
	mustHave(t, "replace X", out, "Y", "take their value from it")
	out = call(t, cs, "edit_symbol", map[string]any{"action": "delete", "name": "X", "file_path": "esym/s.go"})
	mustHave(t, "delete X", out, "Not deleted", "rely on iota or on a repeated expression")
	if got := readFile(t, root, "esym/s.go"); !strings.Contains(got, `X = "a"`) {
		t.Fatalf("refused edits wrote the file:\n%s", got)
	}

	// Deleting the first constant of an iota group would shift the rest: refused as before (own package,
	// since esym already declares Red).
	writeFile(t, root, "esymd/d.go", "package esymd\n\nconst (\n\tRed = iota\n\tGreen\n\tBlue\n)\n")
	out = call(t, cs, "edit_symbol", map[string]any{"action": "delete", "name": "Red", "file_path": "esymd/d.go"})
	mustHave(t, "delete Red", out, "Not deleted", "rely on iota")
	if got := readFile(t, root, "esymd/d.go"); !strings.Contains(got, "Red = iota") {
		t.Fatalf("refused delete wrote the file:\n%s", got)
	}
}

// ESYM-002: a replacement whose source declares other names is refused when it lacks the replaced
// name, and listed in the header when it has it.
func TestEditSymbolReplaceMultiDeclaration(t *testing.T) {
	cs, root := session(t, nil)
	const before = "package multi\n\nfunc Parse(s string) int { return len(s) }\n"
	writeFile(t, root, "multi/p.go", before)
	out := call(t, cs, "edit_symbol", map[string]any{"action": "replace", "name": "Parse", "file_path": "multi/p.go", "source": "func A() {}\n\nfunc helperA() {}"})
	mustHave(t, "replace without the name", out, "source declares A, helperA but not Parse; replace expects the new definition of Parse (use insert for extra declarations)")
	if got := readFile(t, root, "multi/p.go"); got != before {
		t.Fatalf("refused replace wrote the file:\n%s", got)
	}
	out = call(t, cs, "edit_symbol", map[string]any{"action": "replace", "name": "Parse", "file_path": "multi/p.go", "source": "func Parse(s string) int { return len(s) + 1 }\n\nfunc helperP() int { return 0 }"})
	mustHave(t, "replace with the name and an extra", out, "replace Parse (also declares helperP)", "Applied")
	if got := readFile(t, root, "multi/p.go"); !strings.Contains(got, "func helperP()") || !strings.Contains(got, "len(s) + 1") {
		t.Fatalf("both declarations should be written:\n%s", got)
	}
}

// A grouped replacement that changes the number of specs in a const group would move the iota values
// of the constants after it, so it is refused; a source with no declarations is refused too.
func TestEditSymbolGroupedConstSpecCount(t *testing.T) {
	cs, root := session(t, nil)
	const before = "package grp\n\nconst (\n\tA = iota\n\tB = iota\n)\n"
	writeFile(t, root, "grp/c.go", before)
	out := call(t, cs, "edit_symbol", map[string]any{"action": "replace", "name": "A", "file_path": "grp/c.go", "source": "A = iota\nX = 9"})
	mustHave(t, "replace adds a spec", out, "would move them")
	out = call(t, cs, "edit_symbol", map[string]any{"action": "replace", "name": "A", "file_path": "grp/c.go", "source": "// gone"})
	mustHave(t, "replace with nothing", out, "source declares nothing")
	if got := readFile(t, root, "grp/c.go"); got != before {
		t.Fatalf("refused replace wrote the file:\n%s", got)
	}
}

// ESYM-002 inside a grouped var block: the replaced name must be declared, extras are listed.
func TestEditSymbolGroupedVarMultiDeclaration(t *testing.T) {
	cs, root := session(t, nil)
	const before = "package vgrp\n\nvar (\n\tva = 1\n\tvb = 2\n)\n"
	writeFile(t, root, "vgrp/v.go", before)
	out := call(t, cs, "edit_symbol", map[string]any{"action": "replace", "name": "va", "file_path": "vgrp/v.go", "source": "vx = 3\nvy = 4"})
	mustHave(t, "replace without the name", out, "source declares vx, vy but not va")
	if got := readFile(t, root, "vgrp/v.go"); got != before {
		t.Fatalf("refused replace wrote the file:\n%s", got)
	}
	out = call(t, cs, "edit_symbol", map[string]any{"action": "replace", "name": "va", "file_path": "vgrp/v.go", "source": "va = 1\nvx = 3"})
	mustHave(t, "replace with the name and an extra", out, "replace va (also declares vx)", "Applied")
}

// A top-level replacement whose source has no declaration is refused, and the file is left alone.
func TestEditSymbolReplaceCommentOnly(t *testing.T) {
	cs, root := session(t, nil)
	const before = "package keep\n\nfunc Keep() int { return 1 }\n"
	writeFile(t, root, "keep/k.go", before)
	out := call(t, cs, "edit_symbol", map[string]any{"action": "replace", "name": "Keep", "file_path": "keep/k.go", "source": "// gone"})
	mustHave(t, "replace with a comment only", out, "source declares nothing")
	if got := readFile(t, root, "keep/k.go"); got != before {
		t.Fatalf("refused replace wrote the file:\n%s", got)
	}
}

// ESYM-009: a replacement without the old doc block also drops the //go: directive, and the note names it.
func TestEditSymbolReplaceNamesDroppedDirective(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "dir/d.go", "package dir\n\n// Parse parses s: the old doc.\n//\n//go:noinline\nfunc Parse(s string) int { return len(s) }\n")
	out := call(t, cs, "edit_symbol", map[string]any{"action": "replace", "name": "Parse", "file_path": "dir/d.go", "source": "func Parse(s string) int {\n\treturn len(s) + 1\n}"})
	mustHave(t, "replace", out, "Applied", "directive //go:noinline is not in source and is dropped")
}

// ESYM-010: deleting or renaming away an exported name notes that code outside the workspace may use it.
func TestEditSymbolExportedNameNote(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "lone/l.go", "package lone\n\nfunc Helper() int { return 1 }\n")
	out := call(t, cs, "edit_symbol", map[string]any{"action": "delete", "name": "Helper", "file_path": "lone/l.go"})
	mustHave(t, "delete exported", out, "Applied", "Helper was exported from example.com/fx/lone; nothing in this workspace uses it, but code outside the workspace that imports lone no longer builds.")

	writeFile(t, root, "lone2/l.go", "package lone2\n\nfunc Helper() int { return 1 }\n")
	out = call(t, cs, "edit_symbol", map[string]any{"action": "replace", "name": "Helper", "file_path": "lone2/l.go", "source": "func Other() int { return 2 }"})
	mustHave(t, "replace away the exported name", out, "Applied", "Helper was exported from example.com/fx/lone2; code outside the workspace that imports lone2 no longer builds.")
}
