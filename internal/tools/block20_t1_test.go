package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/diagdelta"
	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
	"github.com/illescasDaniel/codenav-go-mcp/internal/txn"
)

// REN-006 (review): the over-limit refusal is made after the simulation, with the diagnostics; the plan it stores
// carries the config fingerprint, so a go.mod edit after the refusal makes apply_edit refuse it.
func TestT1LargeRefusalStoresPlanWithConfigFingerprint(t *testing.T) {
	root := t.TempDir()
	goMod := func(v string) {
		if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/t1\n\ngo "+v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	goMod("1.22")
	w := &Workspace{Root: root, Journal: txn.NewJournal(txn.WriteGuard{Root: root}), Store: txn.NewPlanStore()}
	var cs []edits.FileChange
	for i := range txn.DefaultMaxFiles + 1 {
		s := "package f\n"
		cs = append(cs, edits.FileChange{Path: filepath.Join(root, "f", fmt.Sprintf("f%02d.go", i)), New: &s})
	}
	plan := edits.NewPlan("create 41 files", cs)
	plan.ConfigFP = gomod.Fingerprint(root) // set before the write decision, as finishPlanWith does

	_, _, err := conclude(w, plan, diagdelta.Delta{}, true, -1, false, nil, nil)
	if err == nil {
		t.Fatal("an over-limit write was not refused")
	}
	m := regexp.MustCompile(`Preview id (\w+);`).FindStringSubmatch(err.Error())
	if m == nil {
		t.Fatalf("refusal names no preview id: %s", err)
	}
	id := m[1]
	if want := fmt.Sprintf(`apply_edit(id=%q, allow_large=true)`, id); !strings.Contains(err.Error(), want) {
		t.Errorf("refusal lacks %s: %s", want, err)
	}
	if !strings.Contains(err.Error(), "f00.go") {
		t.Errorf("refusal does not carry the diff: %s", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "f")); statErr == nil {
		t.Fatal("the refused edit wrote files")
	}
	stored, gerr := w.Store.Get(id)
	if gerr != nil || stored != plan {
		t.Fatalf("refused plan is not stored under %s: %v", id, gerr)
	}
	if stored.ConfigFP == "" {
		t.Fatal("stored plan has no config fingerprint")
	}
	// apply_edit of the same id names the same preview, not a new one.
	if _, _, err2 := conclude(w, stored, diagdelta.Delta{}, true, -1, false, nil, nil); err2 == nil || !strings.Contains(err2.Error(), "Preview id "+id+";") {
		t.Errorf("second refusal does not reuse %s: %v", id, err2)
	}
	// A go.mod edit after the refusal: the stored plan refuses to apply.
	goMod("1.23")
	if err := w.Journal.CheckFresh(stored); err == nil || !strings.Contains(err.Error(), "Go config changed") {
		t.Errorf("config change after the refusal not caught: %v", err)
	}
}
