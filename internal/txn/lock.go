package txn

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

const lockFileName = "lock"

// lockWait is how long a write waits for another codenav-go-mcp process that is writing in the same
// workspace (a variable so tests can shorten it).
var lockWait = 30 * time.Second

var errLockBusy = errors.New("lock busy")

// lockWrites takes the advisory lock of the workspace's state directory (<LogDir>/lock) so that two
// server processes on one checkout do not interleave their writes. The lock is released by the
// returned function. A lock that cannot be used (no state directory, unsupported file system or OS)
// does not stop the edit: it goes ahead without, and note says so.
func (j *Journal) lockWrites() (unlock func(), note string, err error) {
	if j.LogDir == "" {
		return func() {}, "", nil
	}
	if os.MkdirAll(j.LogDir, 0o755) != nil {
		return func() {}, "", nil // the intent log says that the state directory is unusable
	}
	unlock, err = acquireLock(filepath.Join(j.LogDir, lockFileName), lockWait)
	switch {
	case err == nil:
		return unlock, "", nil
	case errors.Is(err, errLockBusy):
		return nil, "", toolerr.Inputf("another codenav-go-mcp process has been writing in this workspace for over %s (lock %s); try again when it is done", lockWait, filepath.Join(j.LogDir, lockFileName))
	}
	return func() {}, "writes are not serialised with other codenav-go-mcp processes on this workspace: " + err.Error(), nil
}
