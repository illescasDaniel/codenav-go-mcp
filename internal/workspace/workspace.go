// Package workspace decides which directory the server navigates.
package workspace

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Selection is a chosen root and where it came from.
type Selection struct{ Root, Source string }

// Selector picks the workspace. Authority order:
//  1. explicit env set: pinned, never overridden;
//  2. client roots: first that is the base or another worktree of the same repository;
//  3. CLAUDE_PROJECT_DIR, else cwd.
type Selector struct {
	Base, BaseSource string
	envName          string
	Pinned           bool
}

func resolve(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(h, p[1:])
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

func NewSelector(explicitEnv string) *Selector {
	s := &Selector{envName: explicitEnv, Pinned: os.Getenv(explicitEnv) != ""}
	for _, key := range []string{explicitEnv, "CLAUDE_PROJECT_DIR"} {
		if v := os.Getenv(key); v != "" {
			s.Base, s.BaseSource = resolve(v), key
			return s
		}
	}
	wd, _ := os.Getwd()
	s.Base, s.BaseSource = resolve(wd), "cwd"
	return s
}

// IsGoProject reports whether root itself holds a go.mod or a go.work, the files that make it a Go
// workspace root. A directory without either gives gopls no module to load, so symbol answers are empty.
func IsGoProject(root string) bool {
	for _, name := range []string{"go.mod", "go.work"} {
		if st, err := os.Stat(filepath.Join(root, name)); err == nil && !st.IsDir() {
			return true
		}
	}
	return false
}

func (s *Selector) Explain(source string) string {
	switch source {
	case "client roots":
		return "client roots (the MCP client reported this checkout/worktree of the same repository as the configured base " + s.Base + ")"
	case s.envName:
		return "pinned by $" + s.envName + "; client roots are ignored"
	case "CLAUDE_PROJECT_DIR":
		return "$CLAUDE_PROJECT_DIR (default; the client has not reported another checkout/worktree of this repository)"
	}
	return "server working directory ($" + s.envName + " and $CLAUDE_PROJECT_DIR are unset)"
}

// Select picks the root given the client's roots (file URIs; may be nil).
func (s *Selector) Select(rootURIs []string) Selection {
	if s.Pinned {
		return Selection{s.Base, s.BaseSource}
	}
	for _, uri := range rootURIs {
		if u, err := url.Parse(uri); err == nil && u.Scheme == "file" {
			if root := resolve(u.Path); SameRepository(root, s.Base) {
				return Selection{root, "client roots"}
			}
		}
	}
	return Selection{s.Base, s.BaseSource}
}

var (
	commonMu    sync.Mutex
	commonCache = map[string]string{}
)

func gitCommonDir(path string) string {
	commonMu.Lock()
	defer commonMu.Unlock()
	if v, ok := commonCache[path]; ok {
		return v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--git-common-dir").Output()
	res := ""
	if err == nil {
		if c := strings.TrimSpace(string(out)); c != "" {
			if !filepath.IsAbs(c) {
				c = filepath.Join(path, c)
			}
			res = resolve(c)
		}
	}
	commonCache[path] = res
	return res
}

// SameRepository: a and b are the same directory or two checkouts/worktrees of one git repository.
func SameRepository(a, b string) bool {
	if a == b {
		return true
	}
	ca := gitCommonDir(a)
	return ca != "" && ca == gitCommonDir(b)
}
