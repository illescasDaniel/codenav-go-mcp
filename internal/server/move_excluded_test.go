package server

import (
	"os/exec"
	"strings"
	"testing"
)

// BUG-MOVE-001: a //go:embed var moved to another directory would lose its files and the embed import; the
// move is refused (no write, the reason named). Inside its directory the destination gets the embed import.
func TestMoveEmbedVarRefusedAcrossDirectories(t *testing.T) {
	const res = "package emb\n\nimport _ \"embed\"\n\n//go:embed data.txt\nvar Data string\n\nfunc Len() int { return len(Data) }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "emb/res.go", res)
	writeFile(t, root, "emb/data.txt", "hi")
	writeFile(t, root, "emb2/d.go", "package emb2\n\nfunc D() int { return 1 }\n")

	out, isErr := callIsError(t, cs, "move", map[string]any{"name": "Data", "file_path": "emb/res.go", "to_file": "emb2/d.go"})
	mustHave(t, "move embed", out, "has a //go:embed directive", "can only move within emb")
	if !isErr {
		t.Errorf("the refusal must be an error answer:\n%s", out)
	}
	if readFile(t, root, "emb/res.go") != res || strings.Contains(readFile(t, root, "emb2/d.go"), "Data") {
		t.Fatal("a refused move must not write any file")
	}
}

func TestMoveEmbedVarWithinDirectoryBuilds(t *testing.T) {
	const res = "package emb\n\nimport _ \"embed\"\n\n//go:embed data.txt\nvar Data string\n\nfunc Len() int { return len(Data) }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "emb/res.go", res)
	writeFile(t, root, "emb/data.txt", "hi")

	out := call(t, cs, "move", map[string]any{"name": "Data", "file_path": "emb/res.go", "to_file": "emb/more.go"})
	mustHave(t, "move embed within", out, "Applied")
	got := readFile(t, root, "emb/more.go")
	if !strings.Contains(got, "//go:embed data.txt") || !strings.Contains(got, `_ "embed"`) {
		t.Fatalf("the moved var must keep its directive and get the embed import:\n%s", got)
	}
	build := exec.Command("go", "build", "./emb/")
	build.Dir = root
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("the moved tree must build: %v\n%s\n%s", err, b, got)
	}
}

// BUG-MOVE-004: a declaration from a //go:build linux file keeps that constraint in a new destination file.
func TestMoveConstraintCarriedToNewFile(t *testing.T) {
	const tags = "//go:build linux\n\npackage tg\n\nfunc Tg() int { return 1 }\n\nfunc Keep() int { return 2 }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "tg/tags.go", tags)
	writeFile(t, root, "tgdst/d.go", "package tgdst\n\nfunc D() int { return 1 }\n")

	out := call(t, cs, "move", map[string]any{"name": "Tg", "file_path": "tg/tags.go", "to_file": "tgdst/tg.go"})
	mustHave(t, "move constrained", out, "Applied")
	if got := readFile(t, root, "tgdst/tg.go"); !strings.HasPrefix(got, "//go:build linux\n") {
		t.Fatalf("the new file must carry the source's build constraint:\n%s", got)
	}
}

// BUG-MOVE-004: an existing destination without the same constraint would build the moved code everywhere:
// the move is held (a preview that apply_edit writes on request), and nothing is written.
func TestMoveConstraintMismatchHeldForExistingFile(t *testing.T) {
	const tags = "//go:build linux\n\npackage tg\n\nfunc Tg() int { return 1 }\n\nfunc Keep() int { return 2 }\n"
	const dst = "package tgdst\n\nfunc D() int { return 1 }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "tg/tags.go", tags)
	writeFile(t, root, "tgdst/d.go", dst)

	out := call(t, cs, "move", map[string]any{"name": "Tg", "file_path": "tg/tags.go", "to_file": "tgdst/d.go"})
	mustHave(t, "move constrained held", out, "Not written", "built under `linux` in tags.go", "apply_edit")
	if readFile(t, root, "tg/tags.go") != tags || readFile(t, root, "tgdst/d.go") != dst {
		t.Fatal("a held move must not write any file")
	}
	id := previewID(t, out)
	if out := call(t, cs, "apply_edit", map[string]any{"id": id}); !strings.Contains(out, "Applied") {
		t.Fatalf("apply_edit must write the held move:\n%s", out)
	}
	if got := readFile(t, root, "tgdst/d.go"); !strings.Contains(got, "func Tg() int") {
		t.Fatalf("apply_edit must move the declaration:\n%s", got)
	}
}

// BUG-MOVE-002: the destination's windows-only file already declares the name; the linux check cannot see the
// clash, so the move is held and names the excluded file.
func TestMoveHeldForNameDeclaredInExcludedDestinationFile(t *testing.T) {
	const src = "package m2src\n\nfunc Dup() int { return 1 }\n\nfunc Use() int { return Dup() }\n"
	const dst = "package m2dst\n\nfunc Keep() int { return 0 }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "m2src/s.go", src)
	writeFile(t, root, "m2dst/a.go", dst)
	writeFile(t, root, "m2dst/x_windows.go", "package m2dst\n\nfunc Dup() int { return 2 }\n")

	out := call(t, cs, "move", map[string]any{"name": "Dup", "file_path": "m2src/s.go", "to_file": "m2dst/dup.go"})
	mustHave(t, "move excluded clash", out, "Not written", "Dup is already declared in m2dst/x_windows.go:3", "apply_edit")
	if strings.Contains(out, "Applied") {
		t.Fatalf("the clash must not be applied:\n%s", out)
	}
	if readFile(t, root, "m2src/s.go") != src || readFile(t, root, "m2dst/a.go") != dst {
		t.Fatal("a held move must not write any file")
	}
}

// BUG-MOVE-004 twin: tags.go (linux) and tags_windows.go both declare Tg, and main.go (built everywhere) calls it.
// On linux the caller is rewritten to dst.Tg while the windows build of src keeps its own Tg: the move is held,
// and the twin file is named.
func TestMoveHeldForBuildTwinOfMovedName(t *testing.T) {
	const tags = "//go:build linux\n\npackage tw\n\nfunc Tg() int { return 1 }\n"
	const win = "package tw\n\nfunc Tg() int { return 2 }\n"
	const caller = "package tw\n\nfunc Caller() int { return Tg() }\n"
	const dst = "package twdst\n\nfunc D() int { return 1 }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "tw/tags.go", tags)
	writeFile(t, root, "tw/tags_windows.go", win)
	writeFile(t, root, "tw/main.go", caller)
	writeFile(t, root, "twdst/d.go", dst)

	out := call(t, cs, "move", map[string]any{"name": "Tg", "file_path": "tw/tags.go", "to_file": "twdst/tg.go"})
	mustHave(t, "move twin", out, "Not written", "Tg is declared again in files the build leaves out: tw/tags_windows.go:3", "apply_edit")
	if strings.Contains(out, "Applied") {
		t.Fatalf("the twin must hold the move:\n%s", out)
	}
	if readFile(t, root, "tw/tags.go") != tags || readFile(t, root, "tw/tags_windows.go") != win || readFile(t, root, "tw/main.go") != caller || readFile(t, root, "twdst/d.go") != dst {
		t.Fatal("a held move must not write any file")
	}
	id := previewID(t, out)
	if out := call(t, cs, "apply_edit", map[string]any{"id": id}); !strings.Contains(out, "Applied") {
		t.Fatalf("apply_edit must write the held move:\n%s", out)
	}
	if got := readFile(t, root, "tw/main.go"); !strings.Contains(got, "twdst.Tg()") {
		t.Fatalf("apply_edit must rewrite the caller:\n%s", got)
	}
}

// BUG-SIG-003 family: a file the build leaves out still calls the moved name, so it is not rewritten: held.
func TestMoveHeldForExcludedCallerOfMovedName(t *testing.T) {
	const src = "package ex\n\nfunc Dup() int { return 1 }\n\nfunc Keep() int { return 0 }\n"
	const use = "package ex\n\nfunc WinUse() int { return Dup() }\n"
	const dst = "package ex2\n\nfunc D() int { return 1 }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "ex/s.go", src)
	writeFile(t, root, "ex/use_windows.go", use)
	writeFile(t, root, "ex2/d.go", dst)

	out := call(t, cs, "move", map[string]any{"name": "Dup", "file_path": "ex/s.go", "to_file": "ex2/dup.go"})
	mustHave(t, "move excluded caller", out, "Not written", "use(s) of Dup in files the build leaves out are not rewritten: ex/use_windows.go:3", "apply_edit")
	if readFile(t, root, "ex/s.go") != src || readFile(t, root, "ex/use_windows.go") != use || readFile(t, root, "ex2/d.go") != dst {
		t.Fatal("a held move must not write any file")
	}
}

// BUG-MOVE-004 equivalence: a _linux file moved into another _linux file adds no redundant header, and a
// constraint spelled in another order is the same constraint: neither is held.
func TestMoveConstraintEquivalentIsNotHeld(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "sx/x_linux.go", "package sx\n\nfunc Mv() int { return 1 }\n\nfunc Keep() int { return 2 }\n")
	writeFile(t, root, "sy/d.go", "package sy\n\nfunc D() int { return 1 }\n")
	writeFile(t, root, "ord/x_linux_amd64.go", "package ord\n\nfunc Mv2() int { return 1 }\n\nfunc Keep2() int { return 2 }\n")
	writeFile(t, root, "ord2/d.go", "//go:build amd64 && linux\n\npackage ord2\n\nfunc D() int { return 1 }\n")

	out := call(t, cs, "move", map[string]any{"name": "Mv", "file_path": "sx/x_linux.go", "to_file": "sy/y_linux.go"})
	mustHave(t, "suffix to suffix", out, "Applied")
	if got := readFile(t, root, "sy/y_linux.go"); strings.Contains(got, "//go:build") {
		t.Fatalf("a _linux destination needs no build line:\n%s", got)
	}
	out = call(t, cs, "move", map[string]any{"name": "Mv2", "file_path": "ord/x_linux_amd64.go", "to_file": "ord2/d.go"})
	mustHave(t, "reordered constraint", out, "Applied")
}

// BUG-MOVE-004: a file whose legacy // +build lines are ANDed (linux and amd64) moves its declaration into a new
// file that carries both, not linux alone.
func TestMoveTwoPlusBuildLinesCarriedToNewFile(t *testing.T) {
	const tags = "// +build linux\n// +build amd64\n\npackage tp\n\nfunc Tp() int { return 1 }\n\nfunc Keep() int { return 2 }\n"
	cs, root := session(t, nil)
	writeFile(t, root, "tp/tags.go", tags)
	writeFile(t, root, "tpdst/d.go", "package tpdst\n\nfunc D() int { return 1 }\n")

	out := call(t, cs, "move", map[string]any{"name": "Tp", "file_path": "tp/tags.go", "to_file": "tpdst/tp.go"})
	mustHave(t, "move constrained", out, "Applied")
	if got := readFile(t, root, "tpdst/tp.go"); !strings.HasPrefix(got, "//go:build linux && amd64\n") {
		t.Fatalf("the new file must carry both +build constraints:\n%s", got)
	}
}
