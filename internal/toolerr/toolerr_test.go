package toolerr

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// Round 13 (items 2, 5, 6): a language-server failure is never shown raw, with a file:// URI.
func TestFriendlyLSP(t *testing.T) {
	for name, c := range map[string]struct {
		re   lsp.RequestError
		want string
	}{
		"no package data": {lsp.RequestError{Method: "textDocument/hover", Message: "no package data for file:///w/exp/exp.go"},
			"/w/exp/exp.go is not part of any package gopls could load (check go.mod/go.work, build tags)."},
		"no package metadata": {lsp.RequestError{Method: "textDocument/diagnostic", Message: "no package metadata for file file:///w/badmod/x.go"},
			"/w/badmod/x.go is not part of any package gopls could load (check go.mod/go.work, build tags)."},
		"probe": {lsp.RequestError{Method: "textDocument/definition", Message: "no package metadata for file file:///w/empty/.codenav_probe/p.go"},
			"No Go package is loaded in this workspace, so a name from the standard library or a dependency cannot be looked up (check go.mod/go.work)."},
		"other failure": {lsp.RequestError{Method: "textDocument/rename", Message: "cannot rename file:///w/a.go: boom"},
			"gopls could not answer textDocument/rename: cannot rename /w/a.go: boom"},
	} {
		re := c.re
		got, ok := Format(&re)
		if !ok || got != c.want {
			t.Errorf("%s: got %q want %q", name, got, c.want)
		}
		if strings.Contains(got, "file://") || strings.Contains(got, "LSP error") || strings.Contains(got, "codenav_probe") {
			t.Errorf("%s: raw text leaked: %q", name, got)
		}
	}
}

// BUG-SRCH-009: only a valid query with no match is NotFound; a refusal and an unexpected error are not.
func TestNotFoundMarking(t *testing.T) {
	for name, c := range map[string]struct {
		err      error
		notFound bool
	}{
		"not found":         {NotFoundf("No symbol found matching %q.", "X"), true},
		"wrapped not found": {fmt.Errorf("wrap: %w", NotFoundf("No identifier at a.go:1:1.")), true},
		"refusal":           {Inputf("%s is not a .go file.", "x"), false},
		"unexpected":        {errors.New("boom"), false},
	} {
		if got := IsNotFound(c.err); got != c.notFound {
			t.Errorf("%s: IsNotFound=%v, want %v", name, got, c.notFound)
		}
	}
	if got, ok := Format(NotFoundf("No identifier at %s:%d:%d.", "a.go", 2, 3)); !ok || got != "No identifier at a.go:2:3." {
		t.Errorf("NotFoundf text: %q %v", got, ok)
	}
}
