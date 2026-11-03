//go:build !unix

package txn

import "os"

// linkCount: hard links are not counted on this OS, so no file is reported as linked.
func linkCount(os.FileInfo) uint64 { return 1 }
