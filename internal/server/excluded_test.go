package server

import (
	"runtime"
	"strings"
	"testing"
)

// SIG-003: a call site in a file the build leaves out (x_windows.go on linux) is not rewritten by gopls, so
// change_signature holds the write. The held edit is a preview; apply_edit writes it on request.
func TestChangeSignatureHoldsForExcludedCall(t *testing.T) {
	const plat = "package bsig\n\nfunc Plat(n int) int { return n }\n\nfunc Run() int { return Plat(1) }\n"
	const win = "package bsig\n\nfunc WinRun() int { return Plat(2) }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "bsig/plat.go", plat)
	writeFile(t, root, "bsig/x_windows.go", win)
	add := []any{map[string]any{"name": "k", "type": "int", "value": "0"}}

	out := call(t, cs, "change_signature", map[string]any{"name": "bsig.Plat", "add": add})
	mustHave(t, "change_signature held", out, "Not written", "1 use(s) of Plat in files the build leaves out are not rewritten", "bsig/x_windows.go:3", "apply_edit")
	if readFile(t, root, "bsig/plat.go") != plat || readFile(t, root, "bsig/x_windows.go") != win {
		t.Fatal("a held edit must not write any file")
	}

	id := previewID(t, out)
	if out := call(t, cs, "apply_edit", map[string]any{"id": id}); !strings.Contains(out, "Applied") {
		t.Fatalf("apply_edit must write the held edit:\n%s", out)
	}
	if got := readFile(t, root, "bsig/plat.go"); !strings.Contains(got, "Plat(1, 0)") {
		t.Fatalf("apply_edit must rewrite the built call site:\n%s", got)
	}
	if readFile(t, root, "bsig/x_windows.go") != win {
		t.Fatal("the excluded call site is not rewritten, even by apply_edit")
	}
}

// The preview of the same change says up front that apply=true would not write it.
func TestChangeSignaturePreviewWarnsForExcludedCall(t *testing.T) {
	const plat = "package bsig\n\nfunc Plat(n int) int { return n }\n\nfunc Run() int { return Plat(1) }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "bsig/plat.go", plat)
	writeFile(t, root, "bsig/x_windows.go", "package bsig\n\nfunc WinRun() int { return Plat(2) }\n")
	add := []any{map[string]any{"name": "k", "type": "int", "value": "0"}}

	out := call(t, cs, "change_signature", map[string]any{"name": "bsig.Plat", "add": add, "apply": false})
	mustHave(t, "change_signature preview", out, "Warning: with apply=true this edit is not written", "bsig/x_windows.go:3")
	if readFile(t, root, "bsig/plat.go") != plat {
		t.Fatal("a preview must not write")
	}
}

// snapshot is the content of each file, to check that a held write changed none of them.
func snapshot(t *testing.T, root string, files map[string]string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for rel := range files {
		out[rel] = readFile(t, root, rel)
	}
	return out
}

func assertUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	for rel, want := range before {
		if got := readFile(t, root, rel); got != want {
			t.Fatalf("%s changed by a held write:\n%s", rel, got)
		}
	}
}

// asmBed is a package whose addAsm is implemented in assembly for the build arch, with a same-named declaration in
// the other build (the !arch twin). The go tool and gopls see only the arch build.
func asmBed(arch string) map[string]string {
	return map[string]string{
		"lasm/add_arch.go":  "//go:build " + arch + "\n\npackage lasm\n\nfunc addAsm(a, b int) int\n",
		"lasm/add.s":        "#include \"textflag.h\"\n\nTEXT ·addAsm(SB), NOSPLIT, $0-24\n\tRET\n",
		"lasm/add_other.go": "//go:build !" + arch + "\n\npackage lasm\n\nfunc addAsm(a, b int) int { return a + b }\n",
		"lasm/use.go":       "package lasm\n\nfunc Use() int { return addAsm(1, 2) }\n",
	}
}

// LANG-005: rename_symbol on an assembly-backed func writes silently and leaves the .s file and the other build's
// declaration behind. The write is held (a preview), naming both; apply_edit writes it on request.
func TestRenameHoldsAssemblyAndExcludedTwin(t *testing.T) {
	files := asmBed(runtime.GOARCH)
	cs, root := session(t, nil)
	for rel, src := range files {
		writeFile(t, root, rel, src)
	}
	before := snapshot(t, root, files)

	out := call(t, cs, "rename_symbol", map[string]any{"name": "lasm.addAsm", "file_path": "lasm/add_arch.go", "new_name": "sumAsm"})
	mustHave(t, "rename_symbol held", out, "Not written", "TEXT ·addAsm(SB) in assembly is not renamed: lasm/add.s:3",
		"addAsm is declared again in files the build leaves out: lasm/add_other.go:5", "apply_edit")
	assertUnchanged(t, root, before)

	id := previewID(t, out)
	if out := call(t, cs, "apply_edit", map[string]any{"id": id}); !strings.Contains(out, "Applied") {
		t.Fatalf("apply_edit must write the held rename:\n%s", out)
	}
	if got := readFile(t, root, "lasm/use.go"); !strings.Contains(got, "sumAsm(1, 2)") {
		t.Fatalf("apply_edit must rename the built use:\n%s", got)
	}
	if readFile(t, root, "lasm/add.s") != files["lasm/add.s"] {
		t.Fatal("the assembly is not renamed, even by apply_edit")
	}
}

// LANG-005 (change_signature side): the assembly of a body-less func does not follow a new parameter list, so the
// change is held too, and the preview says so.
func TestChangeSignatureHoldsAssembly(t *testing.T) {
	files := asmBed(runtime.GOARCH)
	cs, root := session(t, nil)
	for rel, src := range files {
		writeFile(t, root, rel, src)
	}
	before := snapshot(t, root, files)

	add := []any{map[string]any{"name": "c", "type": "int", "value": "0"}}
	out := call(t, cs, "change_signature", map[string]any{"name": "lasm.addAsm", "file_path": "lasm/add_arch.go", "add": add})
	mustHave(t, "change_signature held", out, "Not written", "TEXT ·addAsm(SB) in assembly is not rewritten: lasm/add.s:3", "apply_edit")
	assertUnchanged(t, root, before)
}

// LANG-005 (no body, nothing found): a func without a body whose implementation no assembly or linkname in the
// package names is still held, because the change does not reach that implementation.
func TestRenameHoldsBodylessFunc(t *testing.T) {
	const src = "package nb\n\nfunc Ext(x int) int\n"
	cs, root := session(t, nil)
	writeFile(t, root, "nb/ext.go", src)

	out := call(t, cs, "rename_symbol", map[string]any{"name": "nb.Ext", "new_name": "Ext2"})
	mustHave(t, "bodyless rename held", out, "Not written", "Ext has no body, so its implementation is elsewhere")
	if readFile(t, root, "nb/ext.go") != src {
		t.Fatal("a held rename must not write")
	}
}

// LANG-005 (linkname): a //go:linkname directive naming the func is not renamed, so the rename is held.
func TestRenameHoldsLinknameDirective(t *testing.T) {
	const src = "package lnk\n\nimport _ \"unsafe\"\n\n//go:linkname hidden\nfunc hidden(x int) int { return x }\n\nfunc Use() int { return hidden(1) }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "lnk/link.go", src)

	out := call(t, cs, "rename_symbol", map[string]any{"name": "lnk.hidden", "new_name": "shown"})
	mustHave(t, "linkname rename held", out, "Not written", "hidden is named by //go:linkname, and the directive is not renamed: lnk/link.go:5 (//go:linkname hidden)")
	if readFile(t, root, "lnk/link.go") != src {
		t.Fatal("a held rename must not write")
	}
}

// LANG-005 (a use in a file the build leaves out): gopls does not rename the call in x_windows.go, so the rename
// is held. A name with no mention in an excluded file is renamed as before.
func TestRenameHoldsExcludedUseOnly(t *testing.T) {
	const plat = "package rnx\n\nfunc Plat(n int) int { return n }\n\nfunc Run() int { return Plat(1) }\n"
	const win = "package rnx\n\nfunc WinRun() int { return Plat(2) }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "rnx/plat.go", plat)
	writeFile(t, root, "rnx/x_windows.go", win)

	out := call(t, cs, "rename_symbol", map[string]any{"name": "rnx.Plat", "new_name": "Plat2"})
	mustHave(t, "rename held", out, "Not written", "1 use(s) of Plat in files the build leaves out are not renamed: rnx/x_windows.go:3 (file name suffix _windows)")
	if readFile(t, root, "rnx/plat.go") != plat || readFile(t, root, "rnx/x_windows.go") != win {
		t.Fatal("a held rename must not write")
	}

	writeFile(t, root, "rnn/plain.go", "package rnn\n\nfunc Plain() int { return 1 }\n")
	writeFile(t, root, "rnn/other_windows.go", "package rnn\n\nfunc Other() int { return 3 }\n")
	out = call(t, cs, "rename_symbol", map[string]any{"name": "rnn.Plain", "new_name": "Plain2"})
	if !strings.Contains(out, "Applied") || strings.Contains(out, "Not written") {
		t.Fatalf("a name no excluded file mentions is renamed as before:\n%s", out)
	}
}

// LANG-005 (interface method): a selector of the interface method in a file the build leaves out is not checked by
// gopls. The write is not held (a selector is only noted), so the preview of change_signature and of rename_symbol
// both name it. An interface method is a method for buildGuard, as a concrete method is.
func TestInterfaceMethodNotesExcludedSelector(t *testing.T) {
	const ps = "package ps\n\ntype I interface{ Pull(n int) int }\n\nfunc Use(i I) int { return i.Pull(1) }\n"
	const win = "package ps\n\nfunc WinUse(i I) int { return i.Pull(2) }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "ps/i.go", ps)
	writeFile(t, root, "ps/i_windows.go", win)
	add := []any{map[string]any{"name": "c", "type": "int", "value": "0"}}

	out := call(t, cs, "change_signature", map[string]any{"name": "ps.I.Pull", "add": add, "apply": false})
	mustHave(t, "change_signature interface selector note", out, "1 selector(s) of Pull in files the build leaves out were not checked: ps/i_windows.go:3")

	out = call(t, cs, "rename_symbol", map[string]any{"name": "ps.I.Pull", "new_name": "Pull2", "apply": false})
	mustHave(t, "rename_symbol interface selector note", out, "1 selector(s) of Pull in files the build leaves out were not checked: ps/i_windows.go:3")
	if readFile(t, root, "ps/i.go") != ps || readFile(t, root, "ps/i_windows.go") != win {
		t.Fatal("a preview must not write")
	}
}

// REN-002: linked=false on an interface method renames only the interface method and its uses. The implementation
// keeps the old name, so the program stops compiling and the default gate refuses; gopls' Rename, which renamed
// the implementation too, is no longer used for it.
func TestRenameLinkedFalseKeepsImplementation(t *testing.T) {
	const src = "package ifc\n\ntype Reader interface{ Read(p []byte) int }\n\ntype A struct{}\n\nfunc (A) Read(p []byte) int { return len(p) }\n\nvar _ Reader = A{}\n\nfunc Use() int {\n\tvar r Reader = A{}\n\treturn r.Read(nil)\n}\n"
	cs, root := session(t, nil)
	writeFile(t, root, "ifc/iface.go", src)

	out := call(t, cs, "rename_symbol", map[string]any{"file_path": "ifc/iface.go", "line": 3, "column": 24, "new_name": "Pull", "linked": false})
	mustHave(t, "linked=false", out, "Not written", "linked=false: only this interface method and its uses are renamed; the types that implement it keep Read")
	if strings.Contains(out, "func (A) Pull") {
		t.Fatalf("linked=false must not rename the implementation:\n%s", out)
	}
	if readFile(t, root, "ifc/iface.go") != src {
		t.Fatal("a refused rename must not write")
	}
}

// REN-005: each refusal names the flag of the tool that gives it: rename_symbol's linked=false, and
// change_signature's include_overrides=false (which has no linked parameter).
func TestRefusalsNameTheirOwnFlag(t *testing.T) {
	const src = "package kind\n\ntype Kind int\n\nfunc (k Kind) String() string { return \"k\" }\n\nfunc Show() string { return Kind(1).String() }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "kind/kind.go", src)

	out, isErr := callIsError(t, cs, "rename_symbol", map[string]any{"name": "Kind.String", "file_path": "kind/kind.go", "new_name": "Name"})
	if !isErr || !strings.Contains(out, "Pass linked=false to change only this declaration") || strings.Contains(out, "include_overrides") {
		t.Fatalf("rename_symbol must name linked=false only:\n%s", out)
	}
	add := []any{map[string]any{"name": "c", "type": "int", "value": "0"}}
	out, isErr = callIsError(t, cs, "change_signature", map[string]any{"name": "Kind.String", "file_path": "kind/kind.go", "add": add})
	if !isErr || !strings.Contains(out, "Pass include_overrides=false to change only this declaration") || strings.Contains(out, "linked=false") {
		t.Fatalf("change_signature must name include_overrides=false only:\n%s", out)
	}
	if readFile(t, root, "kind/kind.go") != src {
		t.Fatal("refused calls must not write")
	}
}
