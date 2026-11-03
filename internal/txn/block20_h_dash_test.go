package txn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A path that starts with '-' is printed after "--", so rm reads it as a path.
func TestRestoreCommandsEndOptionsForDashPaths(t *testing.T) {
	root := t.TempDir()
	logDir := t.TempDir()
	target := filepath.Join(root, "-rf", "new.go")
	in := intent{
		ID: "h20dash", PID: 0, Op: "apply", Description: "edit", Started: time.Now(), State: "writing",
		Files: []intentFileEntry{{Path: target, Kind: "create", BaseHash: "", NewHash: strings.Repeat("b", 64)}},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, intentFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	notice := Leftover(logDir, root)
	if !strings.Contains(notice, "rm -- "+target) {
		t.Errorf("want %q in the notice:\n%s", "rm -- "+target, notice)
	}
}
