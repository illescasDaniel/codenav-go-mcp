package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// BUG-LEAK-004: the output cap never cuts inside a multi-byte character, and the omitted count stays exact.
func TestBlock20NCapOutputKeepsRunesWhole(t *testing.T) {
	// 480 000 bytes and no newline: the cut falls back to maxOutputBytes, inside a 3-byte character.
	in := strings.Repeat("名", 160_000)
	out := capOutput(in)
	const sep = "\n... "
	i := strings.Index(out, sep)
	if i < 0 {
		t.Fatalf("no omission note:\n%.80s", out)
	}
	body := out[:i]
	if !utf8.ValidString(body) || strings.ContainsRune(body, utf8.RuneError) {
		t.Errorf("the cut left a partial character")
	}
	var omitted int
	if _, err := fmt.Sscanf(out[i+len(sep):], "%d more", &omitted); err != nil {
		t.Fatalf("no omitted count: %v", err)
	}
	if len(body)+omitted != len(in) {
		t.Errorf("kept %d + omitted %d != %d", len(body), omitted, len(in))
	}
}

// BUG-SHAPE-009: no pkg.go.dev link for a dotless module path; the standard library keeps its link.
func TestBlock20NDotlessModuleLinkIsDropped(t *testing.T) {
	const mod = "func X()\n\n---\n\n[`x.X` on pkg.go.dev](https://pkg.go.dev/myapp/x#X)"
	if got := tidyHover(mod); strings.Contains(got, "pkg.go.dev") {
		t.Errorf("dotless module link kept:\n%s", got)
	}
	const std = "func Println()\n\n---\n\n[`fmt.Println` on pkg.go.dev](https://pkg.go.dev/fmt#Println)"
	if got := tidyHover(std); !strings.Contains(got, "pkg.go.dev/fmt#Println") {
		t.Errorf("standard library link dropped:\n%s", got)
	}
}

// BUG-ENV-006: under a broken Go configuration the gopls "no views" and "no package" answers name the
// configuration problem instead of build constraints.
func TestBlock20NNavErrNamesBrokenConfiguration(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{
		"go.mod":   "module example.com/n1\n\ngo 1.21\n\nrequire example.com/bogus v9.9.9-nope\n",
		"m1/m1.go": "package m1\n\nfunc M1() int { return 1 }\n",
	})
	w := &Workspace{Root: root}
	err := w.navErr(context.Background(), &lsp.RequestError{Method: "textDocument/hover", Message: "no views"}, "m1/m1.go", 3, 6)
	if err == nil || !strings.Contains(err.Error(), "the Go configuration is broken") || strings.Contains(err.Error(), "no views") {
		t.Errorf("no views under a broken go.mod: %v", err)
	}
	msg := "no package metadata for file://" + filepath.Join(root, "m1/m1.go")
	err = w.navErr(context.Background(), &lsp.RequestError{Method: "textDocument/hover", Message: msg}, "m1/m1.go", 3, 6)
	if err == nil || !strings.Contains(err.Error(), "the Go configuration is broken") || strings.Contains(err.Error(), "build constraints") {
		t.Errorf("no package under a broken go.mod: %v", err)
	}
}
