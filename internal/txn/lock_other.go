//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package txn

import (
	"errors"
	"time"
)

// acquireLock: advisory file locks are not implemented on this OS; the caller goes ahead and says so.
func acquireLock(string, time.Duration) (func(), error) {
	return nil, errors.New("no advisory file lock on this OS")
}
