package server

import (
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
