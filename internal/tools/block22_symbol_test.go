package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/illescasDaniel/codenav-go-mcp/internal/diagdelta"
	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

// b22ResultText joins the text content of a tool result.
func b22ResultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// Block 22 item 5: search_symbol with a language server that has no view of the workspace is a refusal (isError) in
// the tool's words, not the server's raw "no views" error.
func TestBlock22_SearchSymbolNoViewIsRefusal(t *testing.T) {
	t.Setenv("B22_FAKE_SYMBOL_NOVIEW", "1")
	res := b21fRouteResult(t, "search_symbol", map[string]any{"query": "Shape"})
	got := b22ResultText(res)
	if !res.IsError {
		t.Errorf("search_symbol answer = %q, want a refusal (isError)", got)
	}
	if strings.Contains(got, "no views") || strings.Contains(got, "gopls could not answer") {
		t.Errorf("search_symbol answer carries the raw server error: %q", got)
	}
	if !strings.Contains(got, "the language server has no workspace to search") {
		t.Errorf("search_symbol answer = %q, want the no-workspace refusal", got)
	}
}

// Block 22 item 5: a name lookup that names a file (resolveSymbolRO) routes the server's "no views" answer through the
// file's wording (navErr), as the other position reads do.
func TestBlock22_NameLookupNoViewIsRefusal(t *testing.T) {
	t.Setenv("B22_FAKE_SYMBOL_NOVIEW", "1")
	res := b21fRouteResult(t, "callers", map[string]any{"name": "Scale", "file_path": "ok/ok.go"})
	got := b22ResultText(res)
	if !res.IsError || strings.Contains(got, "no views") || !strings.Contains(got, "the language server has no workspace for ok/ok.go") {
		t.Errorf("callers answer = %q (isError %v), want the file's no-workspace refusal", got, res.IsError)
	}
}

// Block 22 item 5: the probe of a name outside the workspace (resolveSymbolRO, resolve.Probe) routes a server error
// through the file's wording too; the probe's failing definition request is the "no views" answer.
func TestBlock22_ProbeErrorIsRefusal(t *testing.T) {
	t.Setenv("B22_FAKE_SYMBOL_EMPTY", "1")
	t.Setenv("B22_FAKE_DEF_NOVIEW", "1")
	res := b21fRouteResult(t, "callers", map[string]any{"name": "json.Marshal", "file_path": "ok/ok.go"})
	got := b22ResultText(res)
	if strings.Contains(got, "no views") || strings.Contains(got, "gopls could not answer") {
		t.Errorf("probe answer carries the raw server error: %q", got)
	}
	if !res.IsError || !strings.Contains(got, "the language server has no workspace for ok/ok.go") {
		t.Errorf("probe answer = %q (isError %v), want the file's no-workspace refusal", got, res.IsError)
	}
}

// Block 22 item 5: symbolNavErr returns the errors that are not "no views" answers unchanged.
func TestBlock22_SymbolNavErrPassesOtherErrors(t *testing.T) {
	w := &Workspace{Root: t.TempDir()}
	in := toolerr.Inputf("a refusal of the caller")
	if got := w.symbolNavErr(context.Background(), in, ""); got != in {
		t.Errorf("symbolNavErr changed an unrelated error: %v", got)
	}
	re := &lsp.RequestError{Method: "workspace/symbol", Code: -32603, Message: "some other failure"}
	if got := w.symbolNavErr(context.Background(), re, ""); got != re {
		t.Errorf("symbolNavErr changed an unrelated server error: %v", got)
	}
	// The lookup's own scratch file: toolerr words "no package is loaded" for it (round13), so it is not rewritten here.
	probe := &lsp.RequestError{Method: "textDocument/definition", Code: -32603, Message: "no package metadata for file://" + w.Root + "/.codenav_probe/p.go"}
	if got := w.symbolNavErr(context.Background(), probe, ""); got != probe {
		t.Errorf("symbolNavErr rewrote the probe's own failure: %v", got)
	}
}

// Block 22 item 4: the diagnostics of an edit preview name the cause of a broken import, through the same lookup as the
// diagnostics tool (explainBrokenImports). Without the wiring the new error keeps the generic wording.
func TestBlock22_DiagdeltaNamesBrokenImportCause(t *testing.T) {
	root, file := b22BrokenModule(t)
	msg := `could not import example.com/b21dep (no required module provides package "example.com/b21dep")`
	client := b22Client(t, root, map[string]string{"B22_FAKE_DIAG": msg, "B21_FAKE_URI": lsp.PathToURI(file)})
	w := &Workspace{Client: client, Root: root}
	causes := &causeNamer{ctx: context.Background(), root: root, known: map[string]string{}}
	entries, _ := collect(context.Background(), w, nil, []string{file}, nil, causes)
	if len(entries) != 1 {
		t.Fatalf("collect = %d entries, want 1", len(entries))
	}
	if !strings.Contains(entries[0].Message, "could not import example.com/b21dep (parsing b21dep/go.mod") {
		t.Errorf("edit preview message = %q, want the cause of the broken import", entries[0].Message)
	}
	// The before and after sides of one comparison word a broken import the same way: nothing is new and fixed at once.
	again, _ := collect(context.Background(), w, nil, []string{file}, nil, causes)
	if d := diagdelta.Diff(entries, again); len(d.New) != 0 || len(d.Fixed) != 0 {
		t.Errorf("same diagnostics on both sides gave new %v and fixed %v", d.New, d.Fixed)
	}
}

// Block 22 item 4: the causes of one comparison are looked up at most maxDeltaCauseLookups times; the other broken
// imports keep the generic wording.
func TestBlock22_DiagdeltaCauseLookupsAreCapped(t *testing.T) {
	root, file := b22BrokenModule(t)
	var lines []string
	for _, p := range []string{"one", "two", "three", "four", "five", "six"} {
		lines = append(lines, `could not import example.com/`+p+` (no required module provides package "example.com/`+p+`")`)
	}
	client := b22Client(t, root, map[string]string{"B22_FAKE_DIAG": strings.Join(lines, "\n"), "B21_FAKE_URI": lsp.PathToURI(file)})
	w := &Workspace{Client: client, Root: root}
	causes := &causeNamer{ctx: context.Background(), root: root, known: map[string]string{}}
	entries, _ := collect(context.Background(), w, nil, []string{file}, nil, causes)
	if causes.lookups != maxDeltaCauseLookups {
		t.Errorf("lookups = %d, want %d", causes.lookups, maxDeltaCauseLookups)
	}
	if last := entries[len(entries)-1].Message; last != lines[len(lines)-1] {
		t.Errorf("the import past the cap changed: %q", last)
	}
}

// b22BrokenModule writes a module whose replaced dependency b21dep has a go.mod that does not parse, and returns the
// root and the file of the module that imports it.
func b22BrokenModule(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod":        "module example.com/b22diag\n\ngo 1.21\n\nrequire example.com/b21dep v0.0.0\n\nreplace example.com/b21dep => ./b21dep\n",
		"b21dep/go.mod": "module example.com/b21dep\n\ngo 1.21\n\nrequire (\n",
		"b21dep/dep.go": "package b21dep\n",
		"ok/ok.go":      "package ok\n\nimport _ \"example.com/b21dep\"\n",
	}
	for rel, text := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "off")
	return root, filepath.Join(root, "ok", "ok.go")
}

// b22Client starts the fake language server of TestB21FakeLSP for root, with the given environment.
func b22Client(t *testing.T, root string, env map[string]string) *lsp.Client {
	t.Helper()
	t.Setenv(b21fLSPEnv, "1")
	for k, v := range env {
		t.Setenv(k, v)
	}
	client := lsp.New(lsp.Options{Root: root, Command: []string{os.Args[0], "-test.run=^TestB21FakeLSP$"}, Language: "go"})
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop() })
	return client
}
