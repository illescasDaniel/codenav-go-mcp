package gomod

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// noGoMsg is the health problem when the go command is not on PATH (gopls and every load need it).
const noGoMsg = "the go command was not found on PATH"

// gopathMsg is the health problem with GO111MODULE=off: gopls does not load GOPATH-mode packages, and `go list -m`
// refuses to run, which used to be reported as a broken configuration.
const gopathMsg = "GOPATH mode is not supported by gopls; add a go.mod"

// gopathMode reports GO111MODULE=off, the go command's GOPATH mode.
func gopathMode() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GO111MODULE")), "off")
}

// Fingerprint identifies the Go configuration that loads the workspace at root: the content of every module file
// ConfigFiles lists (an absent file counts as absent), GOWORK, GOFLAGS, GO111MODULE and GOTOOLCHAIN, and the go command
// PATH resolves to. A load result or health answer made under one fingerprint is stale under another, and a plan
// records the fingerprint it was simulated under so that a change in between is caught before the write.
func Fingerprint(root string) string {
	h := sha256.New()
	fmt.Fprintf(h, "GOWORK=%s\nGOFLAGS=%s\nGO111MODULE=%s\nGOTOOLCHAIN=%s\ngo=%s\n", os.Getenv("GOWORK"), os.Getenv("GOFLAGS"), os.Getenv("GO111MODULE"), os.Getenv("GOTOOLCHAIN"), goCommand())
	files := ConfigFiles(root)
	sort.Strings(files)
	for i, p := range files {
		if i > 0 && files[i-1] == p {
			continue
		}
		if b, err := os.ReadFile(p); err == nil {
			fmt.Fprintf(h, "%s\x00%x\n", p, sha256.Sum256(b))
		} else {
			fmt.Fprintf(h, "%s\x00absent\n", p)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// goCommand is the path of the go command PATH resolves to, "" when there is none.
func goCommand() string {
	p, err := exec.LookPath("go")
	if err != nil {
		return ""
	}
	return p
}

type healthEntry struct {
	key, msg string
}

// healthCache holds the last health answer per workspace root, valid while its Fingerprint holds.
var healthCache = struct {
	sync.Mutex
	byRoot map[string]healthEntry
}{byRoot: map[string]healthEntry{}}

// Health reports why the Go configuration of root does not load, "" when it does: the go command is missing from
// PATH, `go env` or `go list -m` fails (a toolchain that cannot be fetched, a garbled go.mod or go.work, a GOWORK
// file that is missing or not absolute, a go.work `use` directory without a go.mod). A directory with no go.mod at
// all is not a problem. The answer is cached per Fingerprint; a probe that timed out or was cancelled is not cached.
func Health(ctx context.Context, root string) string {
	key := Fingerprint(root)
	healthCache.Lock()
	if e, ok := healthCache.byRoot[root]; ok && e.key == key {
		healthCache.Unlock()
		return e.msg
	}
	healthCache.Unlock()
	msg, conclusive := probeHealth(ctx, root)
	if conclusive {
		healthCache.Lock()
		healthCache.byRoot[root] = healthEntry{key, msg}
		healthCache.Unlock()
	}
	return msg
}

// probeHealth runs the go command's own checks; conclusive is false when none of them finished.
func probeHealth(ctx context.Context, root string) (msg string, conclusive bool) {
	if goCommand() == "" {
		return noGoMsg, true
	}
	if gopathMode() {
		return gopathMsg, true
	}
	if msg, ok := goFailure(ctx, root, "env", "GOWORK", "GOVERSION"); !ok || msg != "" {
		return msg, ok
	}
	return goFailure(ctx, root, "list", "-m")
}

// goFailure runs the go command in root and returns what it complains about ("" when it succeeds or only reports
// that there is no module here). ok is false when the command did not finish.
func goFailure(ctx context.Context, root string, args ...string) (msg string, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return "", true
	}
	if ctx.Err() != nil {
		return "", false
	}
	text := strings.TrimSpace(stderr.String())
	switch {
	case text == "", strings.Contains(text, "go.mod file not found"), strings.Contains(text, "cannot find main module"):
		if text == "" {
			return fmt.Sprintf("go %s failed (%v)", args[0], err), true
		}
		return "", true
	}
	return relativeProblem(text, root), true
}

// relativeProblem shows the go command's message with paths relative to the workspace, at most four lines.
func relativeProblem(msg, root string) string {
	msg = strings.ReplaceAll(msg, root+string(filepath.Separator), "")
	lines := strings.Split(msg, "\n")
	if len(lines) > 4 {
		lines = append(lines[:4], "...")
	}
	// A line ending in ":" introduces the next one ("go: errors parsing go.mod:" + detail).
	var sb strings.Builder
	for i, l := range lines {
		if i > 0 {
			if strings.HasSuffix(strings.TrimSpace(lines[i-1]), ":") {
				sb.WriteString(" ")
			} else {
				sb.WriteString("; ")
			}
		}
		sb.WriteString(l)
	}
	return sb.String()
}
