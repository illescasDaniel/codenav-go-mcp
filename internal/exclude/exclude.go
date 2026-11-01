// Package exclude lists directory names no tool should walk into.
package exclude

import (
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

var dirNames = map[string]bool{
	"node_modules": true, ".git": true, "vendor": true, "dist": true, "build": true,
	".venv": true, "venv": true, "__pycache__": true,
}

// IsDirName reports whether a directory name is skipped.
func IsDirName(name string) bool { return dirNames[name] }

// IsExcluded reports whether path (absolute, or relative to root) has an excluded directory below root.
func IsExcluded(path, root string) bool {
	rel := path
	if r, err := filepath.Rel(root, path); err == nil {
		rel = r
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if dirNames[part] {
			return true
		}
	}
	return false
}

var (
	goCacheOnce sync.Once
	goCacheDir  string
)

// GoCache is the go build cache directory (`go env GOCACHE`), "" when unknown.
func GoCache() string {
	goCacheOnce.Do(func() {
		if out, err := exec.Command("go", "env", "GOCACHE").Output(); err == nil {
			goCacheDir = strings.TrimSpace(string(out))
		}
	})
	return goCacheDir
}

var (
	toolchainOnce sync.Once
	toolchainDirs []string
)

// isToolchainPath: the path is in GOROOT, the module cache or the build cache, which are no part of any
// project, even when they happen to sit next to it.
func isToolchainPath(p string) bool {
	toolchainOnce.Do(func() {
		for _, k := range []string{"GOROOT", "GOMODCACHE", "GOCACHE", "GOPATH"} {
			if out, err := exec.Command("go", "env", k).Output(); err == nil {
				if d := strings.TrimSpace(string(out)); d != "" {
					toolchainDirs = append(toolchainDirs, d)
				}
			}
		}
	})
	for _, d := range toolchainDirs {
		if p == d || strings.HasPrefix(p, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// RelativeTo renders an absolute path the way results show it: relative to root inside it, "../x" (or
// "../../x", however many levels) for a file that shares an ancestor directory (other than "/") with the
// workspace (e.g. a module the workspace replaces, or a go.work member above it), and the path itself for
// anything else (GOROOT, the module cache, unrelated places). The form depends on where the file is, never
// on how far up the shared ancestor lies, so one answer never mixes relative and absolute spellings.
func RelativeTo(path, root string) string {
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	if filepath.IsAbs(path) && !isToolchainPath(path) {
		up := ""
		for dir := root; ; {
			parent := filepath.Dir(dir)
			if parent == dir || parent == string(filepath.Separator) {
				break
			}
			up += "../"
			if rel, err := filepath.Rel(parent, path); err == nil && !strings.HasPrefix(rel, "..") {
				return up + filepath.ToSlash(rel)
			}
			dir = parent
		}
	}
	return filepath.ToSlash(path)
}

// IsCgoGenerated: a file cgo wrote into the go build cache. Nobody wrote it, and its path is of no use
// to anyone, so results never list it.
func IsCgoGenerated(p string) bool {
	cache := GoCache()
	return cache != "" && strings.HasPrefix(p, cache+string(filepath.Separator)) || strings.Contains(filepath.ToSlash(p), "/go-build/")
}

// syntheticPrefixes start the names cgo generates (`_Cfunc_add`, `_cgoexp_<hash>_f`, `_cgo_...`); nobody wrote them.
var syntheticPrefixes = []string{"_cgoexp_", "_cgo_", "_Cfunc_", "_Ctype_", "_Cvar_", "_Cmacro_", "_Cconst_", "__cgofn__"}

// IsSyntheticName: a symbol name cgo generated. Results never list such symbols.
func IsSyntheticName(name string) bool {
	for _, p := range syntheticPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
