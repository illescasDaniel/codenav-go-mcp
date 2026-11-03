package txn

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression (LEAK-001): the restore command printed for an interrupted write runs as printed when the path
// has a space and a quote, and it is labelled with its shell.
func TestLeftoverRestoreCommandRunsWithSpacesAndQuotes(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on PATH")
	}
	root := t.TempDir()
	logDir := t.TempDir()
	target := filepath.Join(root, "dir with 'quote", "a.go")
	in := intent{
		ID: "h20leak001", PID: 0, Op: "apply", Description: "edit", Started: time.Now(), State: "writing",
		Files: []intentFileEntry{{Path: target, Kind: "modify", BaseHash: strings.Repeat("a", 64), NewHash: strings.Repeat("b", 64), Orig: "0"}},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, intentFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	notice := Leftover(logDir, root)
	if !strings.Contains(notice, "(bash/zsh quoting)") {
		t.Fatalf("the restore command is not labelled with its shell:\n%s", notice)
	}
	var cp string
	for _, l := range strings.Split(notice, "\n") {
		if strings.HasPrefix(l, "  cp ") {
			cp = strings.TrimPrefix(l, "  ")
		}
	}
	if cp == "" {
		t.Fatalf("no cp line in the notice:\n%s", notice)
	}
	// The destination is the last argument; the shell must hand it over as one word, quote included.
	quoted := `'` + strings.ReplaceAll(target, "'", `'\''`) + `'`
	if !strings.HasSuffix(cp, " "+quoted) {
		t.Fatalf("destination not quoted as %s in:\n%s", quoted, cp)
	}
	// Run the printed destination word through sh and compare it with the path.
	out, err := exec.Command("sh", "-c", "printf '%s' "+quoted).Output()
	if err != nil || string(out) != target {
		t.Errorf("shell read %q (err %v), want %q", out, err, target)
	}
}

// An ordinary path stays unquoted, so the notice reads as before for usual workspaces.
func TestShellArgLeavesPlainPathsAlone(t *testing.T) {
	if got := shellArg("/home/me/proj/a.go"); got != "/home/me/proj/a.go" {
		t.Errorf("shellArg plain = %q", got)
	}
}
