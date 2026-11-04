package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
	"github.com/illescasDaniel/codenav-go-mcp/internal/notices"
)

// b21fLSPEnv switches the test binary into the fake language server of TestB21FakeLSP.
const b21fLSPEnv = "B21_FAKE_LSP"

// TestB21FakeLSP is a fake language server. It runs only as the child process of a test (b21fLSPEnv set) and answers
// initialize, workspace/symbol (a symbol named as the query, in the file of B21_FAKE_URI), and fails the position
// requests with the "no views" error the language server gives for a file it has no view of.
func TestB21FakeLSP(t *testing.T) {
	if os.Getenv(b21fLSPEnv) == "" {
		t.Skip("fake language server: only runs as a child process of a test")
	}
	in := bufio.NewReader(os.Stdin)
	docSymbols := 0
	for {
		body, err := b21fReadFrame(in)
		if err != nil {
			os.Exit(0) // the client closed the pipe: the fake server is done
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &msg) != nil || msg.ID == nil || msg.Method == "" {
			continue // notifications need no answer
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
		switch msg.Method {
		case "initialize":
			resp["result"] = map[string]any{"capabilities": map[string]any{}}
		case "workspace/symbol":
			var q struct {
				Query string `json:"query"`
			}
			_ = json.Unmarshal(msg.Params, &q)
			if os.Getenv("B22_FAKE_SYMBOL_NOVIEW") != "" { // block 22: the server has no view of the workspace
				resp["error"] = map[string]any{"code": -32603, "message": "no views for " + os.Getenv("B21_FAKE_URI")}
				break
			}
			if os.Getenv("B22_FAKE_SYMBOL_EMPTY") != "" { // block 22: no symbol of that name in the workspace
				resp["result"] = []map[string]any{}
				break
			}
			resp["result"] = []map[string]any{{
				"name": q.Query, "kind": 12,
				"location": map[string]any{
					"uri":   os.Getenv("B21_FAKE_URI"),
					"range": map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 1}},
				},
			}}
		case "textDocument/documentSymbol":
			docSymbols++
			switch {
			case os.Getenv("B21_FAKE_FAIL_ONCE") == "" || docSymbols > 2:
				resp["error"] = map[string]any{"code": -32603, "message": "no views for " + os.Getenv("B21_FAKE_URI")}
			case docSymbols == 1:
				resp["error"] = map[string]any{"code": -1, "message": "server stopped"}
			default:
				resp["result"] = []map[string]any{{
					"name": "Answered", "kind": 12,
					"range":          map[string]any{"start": map[string]any{"line": 4, "character": 0}, "end": map[string]any{"line": 4, "character": 30}},
					"selectionRange": map[string]any{"start": map[string]any{"line": 4, "character": 5}, "end": map[string]any{"line": 4, "character": 11}},
				}}
			}
		case "textDocument/definition": // block 22: the probe of a name outside the workspace fails with "no views"
			if os.Getenv("B22_FAKE_DEF_NOVIEW") != "" {
				resp["error"] = map[string]any{"code": -32603, "message": "no views for " + os.Getenv("B21_FAKE_URI")}
			}
		case "textDocument/diagnostic": // block 22: one error per line of B22_FAKE_DIAG
			items := []map[string]any{}
			for _, m := range strings.Split(os.Getenv("B22_FAKE_DIAG"), "\n") {
				if m != "" {
					items = append(items, map[string]any{"range": map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 1}}, "severity": 1, "message": m})
				}
			}
			resp["result"] = map[string]any{"kind": "full", "items": items}
		case "textDocument/prepareCallHierarchy", "textDocument/implementation":
			resp["error"] = map[string]any{"code": -32603, "message": "no views for " + os.Getenv("B21_FAKE_URI")}
		default:
			resp["result"] = nil
		}
		payload, _ := json.Marshal(resp)
		fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(payload), payload)
	}
}

// b21fReadFrame reads one LSP message: the headers, then Content-Length bytes.
func b21fReadFrame(r *bufio.Reader) ([]byte, error) {
	n := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	if n < 0 {
		return nil, errors.New("no Content-Length header")
	}
	body := make([]byte, n)
	_, err := io.ReadFull(r, body)
	return body, err
}

// b21fLSPHost hands out one fixed workspace.
type b21fLSPHost struct{ w *Workspace }

func (h b21fLSPHost) Acquire(context.Context, *mcp.ServerSession) (*Workspace, error) {
	return h.w, nil
}
func (h b21fLSPHost) Notes() *notices.Board        { return notices.NewBoard("test") }
func (h b21fLSPHost) Explain(source string) string { return "" }

// b21fRouteCall runs one read tool of RegisterNav against the fake language server and returns the text of the answer.
func b21fRouteCall(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, c := range b21fRouteResult(t, tool, args).Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// b21fRouteResult is b21fRouteCall with the whole result, isError included (block 22).
func b21fRouteResult(t *testing.T, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/b21route\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "ok", "ok.go")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("package ok\n\ntype Shape interface{ Area() int }\n\nfunc Scale(n int) int { return n }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(b21fLSPEnv, "1")
	t.Setenv("B21_FAKE_URI", lsp.PathToURI(file))
	client := lsp.New(lsp.Options{Root: root, Command: []string{os.Args[0], "-test.run=^TestB21FakeLSP$"}, Language: "go"})
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop() })

	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	RegisterNav(s, b21fLSPHost{w: &Workspace{Client: client, Root: root}})
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// The language server's "no views" error is a refusal in the tool's words, for each tool that reads the file's
// position through the server: outline, callers and implementations (navErr). Reverting one of them to the raw error
// shows the server's wording instead, which the test rejects.
func TestBlock21F3_NavErrRoutesPositionReads(t *testing.T) {
	want := "the language server has no workspace for"
	cases := map[string]map[string]any{
		"outline":         {"file_path": "ok/ok.go"},
		"implementations": {"name": "Shape", "file_path": "ok/ok.go"},
		"callers":         {"name": "Scale", "file_path": "ok/ok.go"},
	}
	for tool, args := range cases {
		t.Run(tool, func(t *testing.T) {
			got := b21fRouteCall(t, tool, args)
			if !strings.Contains(got, want) {
				t.Errorf("%s answer = %q, want the refusal %q", tool, got, want)
			}
			if strings.Contains(got, "no views") {
				t.Errorf("%s answer carries the raw server error: %q", tool, got)
			}
		})
	}
}

// The read path of add() is wired through retryAfterRestart: when the server loses the first outline request (code -1),
// the read runs once more and its answer is the one returned. The fake server (B21_FAKE_FAIL_ONCE) loses the first
// documentSymbol and answers the second with the symbol "Answered".
func TestBlock21F3_AddRetriesLostRead(t *testing.T) {
	t.Setenv("B21_FAKE_FAIL_ONCE", "1")
	got := b21fRouteCall(t, "outline", map[string]any{"file_path": "ok/ok.go"})
	if !strings.Contains(got, "Answered") {
		t.Fatalf("outline answer = %q, want the answer of the second attempt", got)
	}
	if strings.Contains(got, "server stopped") {
		t.Errorf("outline answer carries the lost request: %q", got)
	}
}
