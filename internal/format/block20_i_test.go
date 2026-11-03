package format

import (
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// twoGoFilesDump is a cgo failure whose gcc report names b.go before a.go, both files in the same package.
const twoGoFilesDump = "# example.com/diag/two\ncgo: gcc did not produce error at completed:1\nfull error output:\nb.go:9:4: error: in b\n    9 | x\na.go:2:1: error: in a\n    2 | y"

func twoGoFilesItem() lsp.Diagnostic {
	return lsp.Diagnostic{Severity: 1, Source: "go list", Message: twoGoFilesDump}
}

// Regression: with a.go diagnosed, b.go's gcc error must not take the position, and a.go's own error gives it.
func TestDumpWithTwoGoFilesIsPlacedInTheDiagnosedFile(t *testing.T) {
	out := FormatDiagnosticsIn([]lsp.Diagnostic{twoGoFilesItem()}, 10, "", "a.go")
	if !strings.HasPrefix(out, "2:1 [error go list] cgo: error: in a") {
		t.Fatalf("a.go's error must give the position 2:1, got:\n%s", out)
	}
	if !strings.Contains(out, "b.go:9:4: error: in b") {
		t.Errorf("b.go's error must stay in the text, got:\n%s", out)
	}
}

// Regression: with the file unknown, the position never moves and both file names are kept in the message.
func TestDumpWithUnknownFileKeepsPositionAndNames(t *testing.T) {
	out := FormatDiagnosticsIn([]lsp.Diagnostic{twoGoFilesItem()}, 10, "", "")
	if !strings.HasPrefix(out, "1:1 [error go list] cgo: b.go:9:4: error: in b") {
		t.Fatalf("unknown file must keep the position 1:1 and b.go's name, got:\n%s", out)
	}
	if !strings.Contains(out, "a.go:2:1: error: in a") {
		t.Errorf("a.go's name must be kept in the text, got:\n%s", out)
	}
}

// The unknown-file FormatDiagnostics path (used by callers that do not pass a file) behaves the same way.
func TestFormatDiagnosticsWithoutFileNeverRelocates(t *testing.T) {
	out := FormatDiagnostics([]lsp.Diagnostic{twoGoFilesItem()}, 10, "")
	if !strings.HasPrefix(out, "1:1 [error go list] cgo: b.go:9:4: error: in b") {
		t.Fatalf("got:\n%s", out)
	}
}
