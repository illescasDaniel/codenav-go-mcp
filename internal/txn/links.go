package txn

import (
	"fmt"
	"os"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
)

// HardLinkNotes says which modified files of p have other names (hard links). The atomic write gives the edited
// name a new file, so those names keep the old content: the edit is still made, and the answer says so.
func HardLinkNotes(p *edits.EditPlan, root string) []string {
	var out []string
	for _, c := range p.Changes {
		if c.Old == nil || c.New == nil {
			continue
		}
		st, err := os.Stat(c.Path)
		if err != nil {
			continue
		}
		if n := linkCount(st); n > 1 {
			out = append(out, fmt.Sprintf("%s had %d hard links; the atomic replace gives it a new inode, so the other names keep the old content", edits.RelativeName(c.Path, root), n))
		}
	}
	return out
}
