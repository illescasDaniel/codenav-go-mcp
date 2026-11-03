package txn

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
)

// CONC-001 / BED-001: a plan simulated under one Go configuration is refused once go.mod changed, even though
// none of the files it writes did.
func TestCheckFreshRefusesConfigChange(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), "module example.com/c\n\ngo 1.21\n")
	j := NewJournal(WriteGuard{Root: root})

	p := &edits.EditPlan{Previewed: true, ConfigFP: gomod.Fingerprint(root)}
	if err := j.CheckFresh(p); err != nil {
		t.Fatalf("unchanged configuration: %v", err)
	}

	write(t, filepath.Join(root, "go.mod"), "module example.com/c\n\ngo 1.22\n")
	err := j.CheckFresh(p)
	if err == nil || !strings.Contains(err.Error(), "Go config changed since the preview") {
		t.Errorf("previewed plan after go.mod changed: want the config refusal, got %v", err)
	}

	p.Previewed = false
	err = j.CheckFresh(p)
	if err == nil || !strings.Contains(err.Error(), "while the edit was being applied") {
		t.Errorf("unpreviewed plan after go.mod changed: want the in-flight refusal, got %v", err)
	}

	// A plan made before the fingerprint was recorded is not checked against it.
	if err := j.CheckFresh(&edits.EditPlan{}); err != nil {
		t.Errorf("plan without a recorded fingerprint: %v", err)
	}
}
