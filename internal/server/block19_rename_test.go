package server

import (
	"strings"
	"testing"
)

// NAVN-010: a workspace package named like a standard-library one is read as the standard library only when that
// package declares the member. log.Zonk (a local zq/log with Zonk) resolves to the workspace's Zonk; the write tool
// does not refuse it either. json.Marshal (a local zj/json with Marshal) still refuses, with both paths named.
func TestZonkInALocalLogPackageIsNotAStdlibReading(t *testing.T) {
	cs, root := session(t, nil)
	writeFile(t, root, "zq/log/log.go", "package log\n\nfunc Zonk() int { return 1 }\n")
	writeFile(t, root, "zj/json/json.go", "package json\n\nfunc Marshal() int { return 1 }\n")

	out := squash(call(t, cs, "symbol_info", map[string]any{"name": "log.Zonk", "include_references": false}))
	if !strings.Contains(out, "Zonk [function]") || strings.Contains(out, "standard library") || strings.Contains(out, "declares no") {
		t.Errorf("log.Zonk must resolve to the local Zonk:\n%s", out)
	}
	out = call(t, cs, "rename_symbol", map[string]any{"name": "log.Zonk", "new_name": "Zonked", "apply": false})
	if !strings.Contains(out, "Zonked") || strings.Contains(out, "standard library") {
		t.Errorf("rename of log.Zonk must not be refused as ambiguous:\n%s", out)
	}
	out = call(t, cs, "symbol_info", map[string]any{"name": "json.Marshal", "include_references": false})
	if !strings.Contains(out, "encoding/json.Marshal") || !strings.Contains(out, "example.com/fx/zj/json.Marshal") {
		t.Errorf("json.Marshal names a standard-library member and a local one: both paths must be offered:\n%s", out)
	}
}

// REN-001: renaming the receiver s of Start (line 5, column 7) changes that method only; Port and check keep s.
func TestRenameReceiverChangesOnlyItsMethod(t *testing.T) {
	cs, root := session(t, nil)
	const src = "package recv\n\ntype Server struct{ port int }\n\nfunc (s *Server) Start() { s.port = 1 }\n\nfunc (s *Server) check() bool { return s.port > 0 }\n\nfunc (s *Server) Port() int { return s.port }\n"
	writeFile(t, root, "recv/recv.go", src)
	out := call(t, cs, "rename_symbol", map[string]any{"file_path": "recv/recv.go", "line": 5, "column": 7, "new_name": "srv"})
	if !strings.Contains(out, "Applied") {
		t.Fatalf("receiver rename:\n%s", out)
	}
	got := readFile(t, root, "recv/recv.go")
	for _, want := range []string{"func (srv *Server) Start() { srv.port = 1 }", "func (s *Server) check() bool { return s.port > 0 }", "func (s *Server) Port() int { return s.port }"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q after the receiver rename:\n%s", want, got)
		}
	}
}

// REN-003: renaming an interface method whose stdlib implementers the rename leaves alone is held, and change_signature
// is held the same way; the file stays as it was until apply_edit.
func TestForeignImplementersHoldChangeSignature(t *testing.T) {
	cs, root := session(t, nil)
	const src = "package shapes\n\nimport \"os\"\n\nvar _ = os.Getpid\n\ntype Namer interface{ Name() string }\n\ntype Pt struct{}\n\nfunc (Pt) Name() string { return \"\" }\n"
	writeFile(t, root, "shapes/namer.go", src)
	out := call(t, cs, "change_signature", map[string]any{"name": "Namer.Name", "add": []map[string]any{{"name": "ctx", "type": "int", "value": "0"}}})
	if !strings.Contains(out, "Not written") || !strings.Contains(out, "outside the workspace") {
		t.Fatalf("change_signature must hold for the foreign implementers:\n%s", out)
	}
	if got := readFile(t, root, "shapes/namer.go"); got != src {
		t.Errorf("a held change_signature must not change the file:\n%s", got)
	}
	if strings.Contains(out, "/src/") {
		t.Errorf("the hold must name types, not paths:\n%s", out)
	}
}

// REN-004: renaming the embedded alias field A (line 8, column 2) renames the alias A, but sub.B is another package's
// identifier: it stays, and the write applies with no new error.
func TestRenameEmbeddedAliasKeepsForeignQualifier(t *testing.T) {
	cs, root := session(t, nil)
	const sub = "package sub\n\ntype B struct{ N int }\n"
	writeFile(t, root, "sub/sub.go", sub)
	writeFile(t, root, "p023/p.go", "package p023\n\nimport \"example.com/fx/sub\"\n\ntype A = sub.B\n\ntype S struct {\n\tA\n}\n\nfunc Use() int { s := S{A: A{N: 1}}; return s.N }\n")
	out := call(t, cs, "rename_symbol", map[string]any{"file_path": "p023/p.go", "line": 8, "column": 2, "new_name": "Alias"})
	if !strings.Contains(out, "Applied") {
		t.Fatalf("rename of the embedded alias:\n%s", out)
	}
	got := readFile(t, root, "p023/p.go")
	if !strings.Contains(got, "type Alias = sub.B") || strings.Contains(got, "sub.Alias") {
		t.Errorf("sub.B must stay and the alias must be renamed:\n%s", got)
	}
	if other := readFile(t, root, "sub/sub.go"); other != sub {
		t.Errorf("another package's file changed:\n%s", other)
	}
}
