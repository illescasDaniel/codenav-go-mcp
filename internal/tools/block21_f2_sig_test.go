package tools

import (
	"errors"
	"testing"
)

// Regression (SIG-001 follow-up): satisfactionNotes reads each source once. The loops revisit the same members, so
// the reader is called once per path however often a text is asked for.
func TestBlock21F2_TextCacheReadsEachFileOnce(t *testing.T) {
	reads := map[string]int{}
	c := newTextCache(func(p string) (string, error) {
		reads[p]++
		return "src of " + p, nil
	})
	for i := 0; i < 3; i++ {
		for _, p := range []string{"a.go", "b.go", "c.go"} {
			s, err := c.text(p)
			if err != nil || s != "src of "+p {
				t.Fatalf("text(%s) = %q, %v", p, s, err)
			}
		}
	}
	for _, p := range []string{"a.go", "b.go", "c.go"} {
		if reads[p] != 1 {
			t.Errorf("%s read %d times, want 1", p, reads[p])
		}
	}
}

// A read error is returned, and not cached: the next ask reads again.
func TestBlock21F2_TextCacheKeepsErrors(t *testing.T) {
	calls := 0
	c := newTextCache(func(string) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("gone")
		}
		return "ok", nil
	})
	if _, err := c.text("x.go"); err == nil {
		t.Fatal("the first read error was lost")
	}
	if s, err := c.text("x.go"); err != nil || s != "ok" {
		t.Fatalf("after an error the file is read again: %q, %v", s, err)
	}
}
