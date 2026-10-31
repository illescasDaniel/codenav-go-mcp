package diagdelta

import (
	"strings"
	"testing"
)

func TestFriendlyMessageMixedPackages(t *testing.T) {
	got := friendlyMessage("found packages a (x.go) and b (y.go) in /abs/dir")
	if !strings.Contains(got, "declare different packages") || strings.Contains(got, "/abs/dir") {
		t.Errorf("got %q", got)
	}
}
