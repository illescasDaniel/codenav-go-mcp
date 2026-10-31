package gomod

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var problemCache = struct {
	sync.Mutex
	byRoot map[string]problemEntry
}{byRoot: map[string]problemEntry{}}

type problemEntry struct {
	key string
	msg string
}

// configStamp changes whenever a go.mod, the governing go.work or a module listed in it changes, appears
// or disappears.
func configStamp(root string) string {
	var sb strings.Builder
	add := func(p string) {
		sb.WriteString(p)
		if st, err := os.Stat(p); err == nil {
			sb.WriteString(":" + strconv.FormatInt(st.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(st.Size(), 10))
		}
		sb.WriteString("\n")
	}
	sb.WriteString(os.Getenv("GOWORK") + "|" + os.Getenv("GOFLAGS") + "\n")
	for d := root; ; d = filepath.Dir(d) {
		add(filepath.Join(d, "go.mod"))
		if filepath.Dir(d) == d {
			break
		}
	}
	if work := findWork(root); work != "" {
		add(work)
		for d := range workUses(work) {
			add(filepath.Join(d, "go.mod"))
		}
	}
	return sb.String()
}

// ConfigProblem returns the error the go command reports for the go.mod / go.work that govern root
// (a garbled go.mod, a `use` directory without a go.mod, ...), "" when they load. A directory without
// any go.mod is not a problem. The answer is cached until one of those files changes.
func ConfigProblem(ctx context.Context, root string) string {
	key := configStamp(root)
	problemCache.Lock()
	if e, ok := problemCache.byRoot[root]; ok && e.key == key {
		problemCache.Unlock()
		return e.msg
	}
	problemCache.Unlock()
	msg := probeConfig(ctx, root)
	problemCache.Lock()
	problemCache.byRoot[root] = problemEntry{key, msg}
	problemCache.Unlock()
	return msg
}

func probeConfig(ctx context.Context, root string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-m")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil || ctx.Err() != nil {
		return ""
	}
	msg := strings.TrimSpace(stderr.String())
	switch {
	case msg == "", strings.Contains(msg, "go.mod file not found"), strings.Contains(msg, "cannot find main module"):
		return ""
	}
	// Show paths relative to the workspace and cap the length.
	msg = strings.ReplaceAll(msg, root+string(filepath.Separator), "")
	lines := strings.Split(msg, "\n")
	if len(lines) > 4 {
		lines = append(lines[:4], "...")
	}
	return strings.Join(lines, "; ")
}
