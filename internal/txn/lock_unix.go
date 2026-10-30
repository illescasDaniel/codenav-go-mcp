//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package txn

import (
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// acquireLock takes an exclusive flock on path, waiting up to wait; errLockBusy when another holder
// outlasts it. flock locks belong to the open file, so two journals of one process exclude each other too.
func acquireLock(path string, wait time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EINTR {
			_ = f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, errLockBusy
		}
		time.Sleep(20 * time.Millisecond)
	}
}
