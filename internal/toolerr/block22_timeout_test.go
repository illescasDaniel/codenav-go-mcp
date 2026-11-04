package toolerr

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// Block 22 item 1: a plain timeout (gopls was not restarted, it is still busy) must not tell the user to restart
// the server; gopls restarts itself when it stops answering, and the restart case has its own message.
func TestBlock22_PlainTimeoutDoesNotAdviseRestart(t *testing.T) {
	got, ok := Format(fmt.Errorf("symbol_info timed out: %w", context.DeadlineExceeded))
	if !ok {
		t.Fatalf("timeout not formatted")
	}
	if strings.Contains(strings.ToLower(got), "restart") {
		t.Errorf("timeout text tells the user to restart: %q", got)
	}
	if !strings.Contains(got, "busy") || !strings.Contains(got, "retry later") {
		t.Errorf("timeout text should say gopls may be busy and to retry later: %q", got)
	}
}

// Block 22 item 1: the restart case is the Code -1 error from client.go, passed through unchanged.
func TestBlock22_RestartedServerMessagePassesThrough(t *testing.T) {
	re := &lsp.RequestError{Method: "textDocument/hover", Code: -1, Message: "the language server stopped answering and was restarted; retry"}
	got, ok := Format(re)
	if !ok || got != re.Message {
		t.Errorf("got %q (%v), want the restart message unchanged", got, ok)
	}
}
