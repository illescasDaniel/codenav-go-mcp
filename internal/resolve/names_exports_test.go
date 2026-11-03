package resolve

import (
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// NAVN-010: a standard-library reading is named only when that package declares the member. log.Zonk with a local
// zq/log is the workspace's Zonk alone, with no refusal and no suggestion of a log.Zonk that does not exist.
func TestQualifierAmbiguousNeedsAStdlibMember(t *testing.T) {
	if got := stdlibExports("encoding/json")["Marshal"]; !got {
		t.Fatalf("encoding/json must export Marshal (GOROOT not readable?)")
	}
	if got := stdlibExports("log")["Zonk"]; got {
		t.Fatalf("log must not export Zonk")
	}
	local := []*lsp.Symbol{{Name: "Zonk", ContainerName: "example.com/zq/log"}}
	if got := qualifierAmbiguous("log.Zonk", local); got != "" {
		t.Errorf("log.Zonk names no standard-library member: refusal %q", got)
	}
	if got := qualifierAmbiguous("log.Println", local); got != "log" {
		t.Errorf("log.Println is declared by the standard library: want the log path, got %q", got)
	}
}
