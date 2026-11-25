package server

import (
	"strings"
	"testing"
)

func TestBatchTruncationPreservesEditIDs(t *testing.T) {
	for _, id := range []string{"Applied (edit id abc123)", "Preview id def456;"} {
		output := truncateBatch(strings.Repeat("x", batchMaxChars+50) + "\n" + id)
		if !strings.Contains(output, id) || !strings.Contains(output, "truncated") {
			t.Fatalf("truncated output lost %q", id)
		}
	}
}
