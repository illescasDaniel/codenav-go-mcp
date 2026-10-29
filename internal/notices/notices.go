// Package notices carries out-of-band messages appended to tool results.
package notices

import (
	"os"
	"strings"
	"sync"
	"time"
)

// Board holds one-shot notices plus a sticky one while the running binary differs from the one started.
type Board struct {
	server string
	exe    string
	start  fileStamp

	mu        sync.Mutex
	pending   []string
	checkedAt time.Time
	stale     bool
}

type fileStamp struct {
	mtime int64
	size  int64
}

func stampOf(path string) fileStamp {
	if st, err := os.Stat(path); err == nil {
		return fileStamp{st.ModTime().UnixNano(), st.Size()}
	}
	return fileStamp{}
}

func NewBoard(server string) *Board {
	exe, _ := os.Executable()
	return &Board{server: server, exe: exe, start: stampOf(exe), checkedAt: time.Now()}
}

func (b *Board) Post(msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range b.pending {
		if p == msg {
			return
		}
	}
	b.pending = append(b.pending, msg)
}

// Annotate appends pending notices to text.
func (b *Board) Annotate(text string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	notes := b.pending
	b.pending = nil
	if b.exe != "" && time.Since(b.checkedAt) >= 2*time.Second {
		b.checkedAt = time.Now()
		b.stale = stampOf(b.exe) != b.start
	}
	if b.stale {
		notes = append(notes, "the "+b.server+" binary changed since this server started; restart the MCP server to use the new version.")
	}
	var sb strings.Builder
	sb.WriteString(text)
	for _, n := range notes {
		sb.WriteString("\n\n[" + b.server + "] " + n)
	}
	return sb.String()
}
