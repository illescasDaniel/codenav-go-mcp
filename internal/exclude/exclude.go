// Package exclude lists directory names no tool should walk into.
package exclude

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var dirNames = map[string]bool{
	"node_modules": true, ".git": true, "vendor": true, "dist": true, "build": true,
	".venv": true, "venv": true, "__pycache__": true,
}

// IsDirName reports whether a directory name is skipped.
func IsDirName(name string) bool { return dirNames[name] }

// writeSkipUnlessGo are names that are ordinary Go package names too (a package in build/, dist/ or venv/).
// The write tools skip them only in a directory that holds no Go files directly; IsDirName still skips them
// everywhere, so the symbol index is unchanged.
var writeSkipUnlessGo = map[string]bool{"build": true, "dist": true, "venv": true}

// IsExcluded reports whether path (absolute, or relative to root) has an excluded directory below root.
// build, dist and venv count only where the directory holds no .go file itself; a file being written
// counts as one of its directory's Go files.
func IsExcluded(path, root string) bool {
	rel := path
	if r, err := filepath.Rel(root, path); err == nil {
		rel = r
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, part := range parts {
		if !dirNames[part] {
			continue
		}
		if writeSkipUnlessGo[part] {
			dir := filepath.Join(append([]string{root}, parts[:i+1]...)...)
			writing := i == len(parts)-2 && strings.HasSuffix(parts[len(parts)-1], ".go")
			if writing || dirHoldsGo(dir) {
				continue
			}
		}
		return true
	}
	return false
}

// dirHoldsGo reports whether dir has a .go file directly inside it.
func dirHoldsGo(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
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

// goBuildTempRE and goCacheShapeRE are fallbacks for cgo output whose temporary root is unknown: a
// go-build<random>/b<nnn>/ work directory, or a build cache's go-build/<2 hex>/ fan-out directory. A bare
// project directory go-build/use.go matches neither.
var (
	goBuildTempRE  = regexp.MustCompile(`(^|/)go-build[0-9a-f]*/b[0-9]+/`)
	goCacheShapeRE = regexp.MustCompile(`/go-build/[0-9a-f]{2}/`)
)

// IsCgoGenerated: a file cgo wrote into the go build cache or into its work directory. Nobody wrote it, and
// its path is of no use to anyone, so results never list it. The work directory is go-build<random> inside
// $GOTMPDIR or, when that is unset, os.TempDir(); both sides are compared with symlinks resolved too, since
// gopls may report /private/var for /var on macOS. A project directory that merely has that name
// (go-build/use.go) is not cgo output.
func IsCgoGenerated(p string) bool {
	if cache := GoCache(); cache != "" && strings.HasPrefix(p, cache+string(filepath.Separator)) {
		return true
	}
	paths := []string{p}
	if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
		paths = append(paths, r)
	}
	for _, root := range tempRoots() {
		for _, c := range paths {
			if inGoBuildDir(c, root) {
				return true
			}
		}
	}
	slashed := filepath.ToSlash(p)
	return goBuildTempRE.MatchString(slashed) || goCacheShapeRE.MatchString(slashed)
}

// tempRoots lists the directories cgo's work directories may sit in, each also with its symlinks resolved.
func tempRoots() []string {
	var roots []string
	for _, d := range []string{os.Getenv("GOTMPDIR"), os.TempDir()} {
		if d == "" {
			continue
		}
		roots = append(roots, d)
		if r, err := filepath.EvalSymlinks(d); err == nil && r != d {
			roots = append(roots, r)
		}
	}
	return roots
}

// inGoBuildDir reports whether p lies in a go-build<random> directory directly inside root.
func inGoBuildDir(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	parts := strings.SplitN(filepath.ToSlash(rel), "/", 2)
	return len(parts) == 2 && strings.HasPrefix(parts[0], "go-build")
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
