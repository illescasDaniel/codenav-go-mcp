package txn

import (
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
)

// Regression (SCALE/T1 follow-up): a preview pushed out by the 20-plan cap says so, with the reason, instead of
// reading as a typo ("unknown or expired"). The newest 20 stay usable and an id never issued stays unknown.
func TestBlock21F2_EvictedPreviewSaysCap(t *testing.T) {
	s := NewPlanStore()
	var ids []string
	for i := 0; i < 25; i++ {
		ids = append(ids, s.Add(edits.NewPlan("edit", nil)))
	}
	for _, id := range ids[:5] {
		_, err := s.Get(id)
		if err == nil {
			t.Fatalf("evicted preview %s still returned", id)
		}
		if !strings.Contains(err.Error(), "cap of 20 pending previews") {
			t.Errorf("evicted preview %s: the refusal does not name the cap: %v", id, err)
		}
		if strings.Contains(err.Error(), "unknown or expired") {
			t.Errorf("evicted preview %s reads as a typo: %v", id, err)
		}
	}
	for _, id := range ids[5:] {
		if _, err := s.Get(id); err != nil {
			t.Errorf("newest previews must stay pending, %s: %v", id, err)
		}
	}
	if _, err := s.Get("deadbeef"); err == nil || !strings.Contains(err.Error(), "unknown or expired") {
		t.Errorf("an id never issued must still say unknown: %v", err)
	}
}
