//go:build unix

package txn

import (
	"os"
	"syscall"
)

// linkCount is how many names (hard links) the file has.
func linkCount(st os.FileInfo) uint64 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(s.Nlink)
	}
	return 1
}
