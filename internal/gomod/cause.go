package gomod

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/mod/modfile"
)

// causeTimeout bounds one `go list` run of ImportCause; a timeout means "unknown".
const causeTimeout = 10 * time.Second

// maxCauseLen caps the cause text: one line of the go command's message, never a dump.
const maxCauseLen = 240

// maxCauseEntries bounds the cache; it is emptied when full (the entries are cheap to recompute).
const maxCauseEntries = 256

// causeCache holds the answers of ImportCause per causeKey.
var causeCache = struct {
	sync.Mutex
	byKey map[string]string
}{byKey: map[string]string{}}

// ImportCause names the real reason the import path importPath does not resolve for file, when the go command knows
// one: a replaced module whose go.mod does not parse, or a go.sum entry that is missing. It returns "" when the cause is
// unknown (or the go command does not say), and the caller keeps its generic wording: an unknown cause is never invented.
//
// The go command runs in the directory of the module that owns file (the nearest go.mod above it, root when there is
// none), so a nested module or a replaced directory outside root is looked at with its own go.mod and go.sum. The
// command is `go list -e -mod=readonly -json=ImportPath,Error,DepsErrors <importPath>`: it reports both causes through
// the package's Error and DepsErrors, without a build and without the package's directory on disk (a probe file is only
// a gopls overlay). -mod=readonly keeps it from writing go.sum. Paths in the cause are relative to root.
//
// The answer is cached per causeKey; a timed out or failed run is not cached.
func ImportCause(ctx context.Context, root, file, importPath string) string {
	dir := root
	if o, ok := Owner(filepath.Dir(file)); ok {
		dir = o.Dir
	}
	key := causeKey(root, dir, importPath)
	causeCache.Lock()
	msg, ok := causeCache.byKey[key]
	causeCache.Unlock()
	if ok {
		return msg
	}
	msg, conclusive := runCause(ctx, dir, root, importPath)
	if conclusive {
		causeCache.Lock()
		if len(causeCache.byKey) >= maxCauseEntries {
			causeCache.byKey = map[string]string{}
		}
		causeCache.byKey[key] = msg
		causeCache.Unlock()
	}
	return msg
}

// causeKey identifies one lookup: the configuration of root and of the owning module, the import path, and the content
// of the go.mod and go.sum files of the local directories the owning go.mod replaces modules with (a fixed replaced
// go.mod must not keep its old cause; Fingerprint does not cover those files).
func causeKey(root, dir, importPath string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00", Fingerprint(root), Fingerprint(dir), importPath)
	for _, p := range replaceFiles(dir) {
		if b, err := os.ReadFile(p); err == nil {
			fmt.Fprintf(h, "%s\x00%x\n", p, sha256.Sum256(b))
		} else {
			fmt.Fprintf(h, "%s\x00absent\n", p)
		}
	}
	return dir + "\x00" + importPath + "\x00" + hex.EncodeToString(h.Sum(nil))
}

// replaceFiles lists the go.mod and go.sum files of the local directories that the go.mod in dir replaces modules with,
// and those that the go.work governing dir replaces modules with (relative to the go.work's directory). A module
// replaced by a version (not a directory) has no files here.
func replaceFiles(dir string) []string {
	var out []string
	if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
		if f, err := modfile.Parse("go.mod", b, nil); err == nil {
			for _, r := range f.Replace {
				out = append(out, replaceTargetFiles(dir, r)...)
			}
		}
	}
	if work := findWork(dir); work != "" {
		if b, err := os.ReadFile(work); err == nil {
			if f, err := modfile.ParseWork(work, b, nil); err == nil {
				for _, r := range f.Replace {
					out = append(out, replaceTargetFiles(filepath.Dir(work), r)...)
				}
			}
		}
	}
	return out
}

// replaceTargetFiles gives the go.mod and go.sum of a replacement that is a directory, resolved against base.
func replaceTargetFiles(base string, r *modfile.Replace) []string {
	if r.New.Version != "" {
		return nil
	}
	d := r.New.Path
	if !filepath.IsAbs(d) {
		d = filepath.Join(base, d)
	}
	return []string{filepath.Join(d, "go.mod"), filepath.Join(d, "go.sum")}
}

// requiredModules lists the module paths that the go.mod in dir requires.
func requiredModules(dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return nil
	}
	f, err := modfile.Parse("go.mod", b, nil)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range f.Require {
		out = append(out, r.Mod.Path)
	}
	return out
}

// maxCauseOutput caps the go list output one lookup reads; a longer output is not a readable answer.
const maxCauseOutput = 1 << 20

// cappedBuffer keeps the first max bytes written to it and discards the rest. It reports every write as complete, so
// the command is not broken by a closed pipe; over tells that something was discarded.
type cappedBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := c.max - c.buf.Len(); n > room {
		c.over = true
		p = p[:max(room, 0)]
	}
	c.buf.Write(p)
	return n, nil
}

// runCause runs the go command in dir and reads the cause from the package's errors. conclusive is false when the
// command did not finish, its output was too long, or it printed nothing readable (then nothing is cached).
// importPath follows "--", so an import path that starts with a dash is a package name for the go command, never a flag.
func runCause(ctx context.Context, dir, root, importPath string) (msg string, conclusive bool) {
	cctx, cancel := context.WithTimeout(ctx, causeTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "go", "list", "-e", "-mod=readonly", "-json=ImportPath,Error,DepsErrors", "--", importPath)
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	out := &cappedBuffer{max: maxCauseOutput}
	cmd.Stdout = out
	_ = cmd.Run()
	if cctx.Err() != nil || out.over || out.buf.Len() == 0 {
		return "", false
	}
	var p pkg
	if err := json.NewDecoder(&out.buf).Decode(&p); err != nil {
		return "", false
	}
	required := requiredModules(dir)
	for _, e := range append([]*pkgError{p.Error}, p.DepsErrors...) {
		if e == nil || e.Err == "" {
			continue
		}
		if m := causeText(e.Err, importPath, root, required); m != "" {
			return m, true
		}
	}
	return "", true
}

// causeText turns one load error of importPath into its cause, "" when the error is the generic "no module provides
// it" (or something that names no cause). required lists the module paths of the owning go.mod.
func causeText(text, importPath, root string, required []string) string {
	if strings.Contains(text, "missing go.sum entry") {
		return goSumCause(importPath, required)
	}
	first := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	if first == "" || strings.Contains(first, "no required module provides package") ||
		strings.Contains(first, "cannot find module providing package") || strings.Contains(first, "is not in std") ||
		strings.Contains(first, "malformed import path") {
		return ""
	}
	if strings.HasPrefix(first, "module ") {
		if i := strings.Index(first, ": "); i > 0 {
			first = first[i+2:] // "module ./dep: parsing dep/go.mod: ..." names the replacement, the rest is the cause
		}
	}
	return truncateCause(relativePaths(first, root), maxCauseLen)
}

// goSumCause is the cause of a missing go.sum entry for importPath. It names `go get` for the required module the
// import path belongs to (the longest required module path that is a prefix of it); with no such module only
// `go mod tidy` is named, since a path that is not a required module would be a wrong command.
func goSumCause(importPath string, required []string) string {
	best := ""
	for _, m := range required {
		if (importPath == m || strings.HasPrefix(importPath, m+"/")) && len(m) > len(best) {
			best = m
		}
	}
	if best == "" {
		return "missing go.sum entry; run: go mod tidy"
	}
	return "missing go.sum entry; run: go mod tidy, or go get " + best
}

// absPathRE finds an absolute path at the start of a word (a path with a "/" inside a relative one is not matched).
var absPathRE = regexp.MustCompile(`(^|[\s("'\[])(/[^\s:;,()"'\[\]]+)`)

// relativePaths shows the absolute paths in s relative to root (the real path of root too, since the go command prints
// resolved paths). A path outside root keeps a relative form with at most two levels up; anything further stays.
func relativePaths(s, root string) string {
	roots := []string{root}
	if real, err := filepath.EvalSymlinks(root); err == nil && real != root {
		roots = []string{real, root}
	}
	return absPathRE.ReplaceAllStringFunc(s, func(m string) string {
		prefix := ""
		if m[0] != '/' {
			prefix, m = m[:1], m[1:]
		}
		for _, r := range roots {
			rel, err := filepath.Rel(r, m)
			if err != nil {
				continue
			}
			if !strings.HasPrefix(rel, "..") || strings.Count(rel, "..") <= 2 {
				return prefix + filepath.ToSlash(rel)
			}
		}
		return prefix + m
	})
}

// truncateCause cuts s to at most n bytes at a rune boundary, adding "...".
func truncateCause(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
