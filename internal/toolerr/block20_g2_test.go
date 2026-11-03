package toolerr

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// LEAK-002: gopls' rename fallbacks never show its internal type name or its own wording.
func TestBlock20G2RenameFallbacksAreFriendly(t *testing.T) {
	for name, c := range map[string]struct {
		msg, want string
	}{
		"not a function type": {"parsed signature is *ast.Ident, not a function type",
			"nothing renameable at this position"},
		"blank identifier": {`invalid identifier to rename: "_"`,
			`"_" is the blank identifier and cannot be a new name; pick a name.`},
		"other identifier": {`invalid identifier to rename: "1x"`,
			`"1x" is not a valid Go identifier.`},
	} {
		re := lsp.RequestError{Method: "textDocument/rename", Message: c.msg}
		got, ok := Format(&re)
		if !ok || got != c.want {
			t.Errorf("%s: got %q (%v), want %q", name, got, ok, c.want)
		}
		if strings.Contains(got, "*ast") || strings.Contains(got, "gopls") || strings.Contains(got, "%T") {
			t.Errorf("%s: gopls internals leaked: %q", name, got)
		}
	}
}

// NAVP-005: a bodyless declaration says why there is nothing to show, without the LSP method name.
func TestBlock20G2NonGoDefinitionIsFriendly(t *testing.T) {
	re := lsp.RequestError{Method: "textDocument/definition", Message: "can't find non-Go definition of localname"}
	want := "localname is declared without a body (//go:linkname or assembly); no definition to show."
	if got, ok := Format(&re); !ok || got != want {
		t.Errorf("got %q (%v), want %q", got, ok, want)
	}
}

// PROTO-007: a cancelled call is worded as a cancel, not as a timeout, and stays an error.
func TestBlock20G2CancelledCallIsWorded(t *testing.T) {
	const want = "request cancelled"
	for name, err := range map[string]error{
		"bare":    context.Canceled,
		"wrapped": fmt.Errorf("textDocument/references: %w", context.Canceled),
	} {
		got, ok := Format(err)
		if !ok || got != want {
			t.Errorf("%s: got %q (%v), want %q", name, got, ok, want)
		}
	}
}

// A Canceled that comes from somewhere other than the client's cancel is not blamed on the client.
func TestBlock20G2CanceledFromElsewhereDoesNotBlameClient(t *testing.T) {
	got, ok := Format(fmt.Errorf("refresh step: %w", context.Canceled))
	if !ok || got != "request cancelled" || strings.Contains(got, "client") {
		t.Errorf("got %q (%v), want a plain cancel with no client blame", got, ok)
	}
}

// ENV-005 (wording): a hung server is described as possibly loading or stuck, with the restart advice.
func TestBlock20G2TimeoutWording(t *testing.T) {
	const want = "the language server did not answer in time (it may be loading packages or stuck); retry shortly, or restart the server if this repeats"
	for name, err := range map[string]error{
		"20 s request": fmt.Errorf("symbol_info timed out: %w", context.DeadlineExceeded),
		"3 s probe":    fmt.Errorf("probe: %w", context.DeadlineExceeded),
	} {
		got, ok := Format(err)
		if !ok || got != want {
			t.Errorf("%s: got %q (%v), want %q (no hard-coded duration)", name, got, ok, want)
		}
	}
}
