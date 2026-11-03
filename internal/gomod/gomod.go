// Package gomod answers package-graph questions through `go list`.
package gomod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
)

// EnvBuildFlags are extra build flags (e.g. -tags=integration) gopls runs with; `go list` must see
// the same ones, or the dependents it finds differ from the ones gopls type-checks.
const EnvBuildFlags = "CODENAV_GO_MCP_BUILD_FLAGS"

type pkg struct {
	ImportPath   string
	Name         string
	Dir          string
	GoFiles      []string
	CgoFiles     []string
	TestGoFiles  []string
	XTestGoFiles []string
	Imports      []string
	TestImports  []string
	XTestImports []string
	Error        *pkgError   // why the package itself does not load
	DepsErrors   []*pkgError // why one of its dependencies does not
}

// pkgError is a load error `go list -e` reports (the go command's PackageError).
type pkgError struct {
	Pos string
	Err string
}

// loadProblems describes why p does not load, one line per error: "import path: position: cause".
func (p pkg) loadProblems() []string {
	var out []string
	add := func(e *pkgError) {
		if e == nil || e.Err == "" {
			return
		}
		cause := strings.SplitN(e.Err, "\n", 2)[0]
		if i := strings.Index(cause, "; to add"); i >= 0 {
			cause = cause[:i]
		}
		line := p.ImportPath + ": "
		if e.Pos != "" {
			line += e.Pos + ": "
		}
		out = append(out, line+cause)
	}
	add(p.Error)
	for _, e := range p.DepsErrors {
		add(e)
	}
	return out
}

func (p pkg) files() []string {
	var out []string
	for _, group := range [][]string{p.GoFiles, p.CgoFiles, p.TestGoFiles, p.XTestGoFiles} {
		for _, f := range group {
			out = append(out, filepath.Join(p.Dir, f))
		}
	}
	return out
}

func (p pkg) importsPkg(path string) bool {
	for _, group := range [][]string{p.Imports, p.TestImports, p.XTestImports} {
		for _, i := range group {
			if i == path {
				return true
			}
		}
	}
	return false
}

// listRuns counts real `go list` runs (tests check the cache with it).
var listRuns atomic.Int64

const listTTL = 2 * time.Minute

type listEntry struct {
	key  string
	at   time.Time
	pkgs []pkg
}

// listCache holds the last `go list` result per directory (the root and the nested modules).
var listCache = struct {
	sync.Mutex
	byDir map[string]listEntry
}{byDir: map[string]listEntry{}}

// fingerprint summarizes everything a `go list` result depends on that we can see cheaply: the
// build flags and GOWORK/GOFLAGS, plus the path, mtime and size of every Go, go.mod and go.work
// file below root (and the governing go.work). "" means do not cache.
func fingerprint(root string, flags []string) string {
	var sb strings.Builder
	sb.WriteString(strings.Join(flags, " ") + "|" + os.Getenv("GOWORK") + "|" + os.Getenv("GOFLAGS") + "|")
	add := func(p string) {
		if st, err := os.Stat(p); err == nil {
			sb.WriteString(p + ":" + strconv.FormatInt(st.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(st.Size(), 10) + "\n")
		}
	}
	if w := findWork(root); w != "" {
		add(w)
	}
	n := 0
	ok := true
	roots := []string{root}
	for _, d := range WorkModules(root) {
		if rel, err := filepath.Rel(root, d); err != nil || strings.HasPrefix(rel, "..") {
			roots = append(roots, d) // a used module outside root: its files matter too
		}
	}
	for _, r := range roots {
		_ = filepath.WalkDir(r, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != r && exclude.IsDirName(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if name := d.Name(); strings.HasSuffix(name, ".go") || name == "go.mod" || name == "go.work" || name == "go.sum" {
				if n++; n > 50000 {
					ok = false
					return filepath.SkipAll
				}
				if info, err := d.Info(); err == nil {
					sb.WriteString(p + ":" + strconv.FormatInt(info.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(info.Size(), 10) + "\n")
				}
			}
			return nil
		})
	}
	if !ok {
		return ""
	}
	return sb.String()
}

// list returns every package of the module(s) under root, cached while the files under root, the
// go.mod/go.work files and the build flags are unchanged (so our own writes and outside edits both
// invalidate it).
func list(ctx context.Context, root string) ([]pkg, error) {
	flags := strings.Fields(os.Getenv(EnvBuildFlags))
	key := fingerprint(root, flags)
	if key != "" {
		listCache.Lock()
		if e, ok := listCache.byDir[root]; ok && e.key == key && time.Since(e.at) < listTTL {
			listCache.Unlock()
			return e.pkgs, nil
		}
		listCache.Unlock()
	}
	pkgs, err := runList(ctx, root, flags)
	if key != "" && err == nil {
		listCache.Lock()
		listCache.byDir[root] = listEntry{key, time.Now(), pkgs}
		listCache.Unlock()
	}
	return pkgs, err
}

// WorkModules lists the directories of the go.work's `use` modules other than root itself (empty
// without a go.work). `go list ./...` and packages.Load("./...") at the root see only the root's own
// module, so these are listed and loaded separately.
func WorkModules(root string) []string {
	work := findWork(root)
	if work == "" {
		return nil
	}
	var out []string
	for d := range workUses(work) {
		if d == filepath.Clean(root) {
			continue
		}
		if st, err := os.Stat(filepath.Join(d, "go.mod")); err == nil && !st.IsDir() {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

func runList(ctx context.Context, root string, flags []string) ([]pkg, error) {
	pkgs, err := runListDir(ctx, root, flags)
	seen := map[string]bool{}
	for _, p := range pkgs {
		seen[p.ImportPath+"\x00"+p.Dir] = true
	}
	for _, d := range WorkModules(root) {
		more, merr := runListDir(ctx, d, flags)
		if merr != nil && len(more) == 0 {
			continue
		}
		for _, p := range more {
			if !seen[p.ImportPath+"\x00"+p.Dir] {
				seen[p.ImportPath+"\x00"+p.Dir] = true
				pkgs = append(pkgs, p)
			}
		}
	}
	return pkgs, err
}

func runListDir(ctx context.Context, root string, flags []string) ([]pkg, error) {
	listRuns.Add(1)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	args := append([]string{"list", "-e"}, flags...)
	args = append(args, "-json=ImportPath,Name,Dir,GoFiles,CgoFiles,TestGoFiles,XTestGoFiles,Imports,TestImports,XTestImports,Error,DepsErrors", "./...")
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil && out.Len() == 0 {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, errors.New("go list did not finish within 60s")
		}
		return nil, err
	}
	var pkgs []pkg
	dec := json.NewDecoder(&out)
	for dec.More() {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			return pkgs, err
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

// Related returns the Go files that can be affected by changing the given files: the other files of
// their packages, and every file of packages importing those packages (including _test packages).
// Edited files come first. Files that are not part of any listed package are returned as given.
// A file of a nested module (a go.mod below root that no go.work lists) is looked up in that module:
// `go list ./...` at the root leaves nested modules out.
func Related(ctx context.Context, root string, edited []string) (files []string, err error) {
	files, _, err = RelatedLoad(ctx, root, edited)
	return files, err
}

// RelatedLoad is Related with the load state of what it looked at. err is the failure of a `go list` whose
// output the importers were searched in (the root's own module, a nested one, or a sibling module), so the
// importers of the edited packages may be missing from files. problems lists the errors of the packages the
// edited files belong to ("import path: position: cause"): an edit to such a package is checked against a graph
// that does not load.
func RelatedLoad(ctx context.Context, root string, edited []string) (files, problems []string, err error) {
	pkgs, err := list(ctx, root)
	nestedDirs := map[string]bool{}
	for _, f := range edited {
		if n, ok := Uncovered(root, f); ok && !nestedDirs[n.Dir] {
			nestedDirs[n.Dir] = true
			more, merr := list(ctx, n.Dir)
			pkgs = append(pkgs, more...)
			err = firstErr(err, merr)
		}
	}
	if len(nestedDirs) > 0 {
		// A module the workspace does not build can be imported by its siblings (a replace directive):
		// their packages are listed too, so the importers of the edited packages are found.
		for _, o := range UncoveredModules(root, exclude.IsDirName, maxSiblingModules) {
			if nestedDirs[o.Dir] {
				continue
			}
			nestedDirs[o.Dir] = true
			more, merr := list(ctx, o.Dir)
			pkgs = append(pkgs, more...)
			err = firstErr(err, merr)
		}
	}
	seen := map[string]bool{}
	add := func(f string) {
		if !seen[f] {
			seen[f] = true
			files = append(files, f)
		}
	}
	for _, f := range edited {
		add(f)
	}
	dirs := map[string]bool{}
	for _, f := range edited {
		dirs[filepath.Dir(f)] = true
	}
	var targets []string
	var same []string
	for _, p := range pkgs {
		if dirs[p.Dir] {
			targets = append(targets, p.ImportPath)
			same = append(same, p.files()...)
			problems = append(problems, p.loadProblems()...)
		}
	}
	sort.Strings(problems)
	problems = uniqueStrings(problems)
	sort.Strings(same)
	for _, f := range same {
		add(f)
	}
	var deps []string
	for _, p := range pkgs {
		if dirs[p.Dir] {
			continue
		}
		for _, t := range targets {
			if p.importsPkg(t) {
				deps = append(deps, p.files()...)
				break
			}
		}
	}
	sort.Strings(deps)
	for _, f := range deps {
		add(f)
	}
	return files, problems, err
}

// firstErr is the first non-nil error of a and b.
func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// uniqueStrings drops repeated entries of a sorted slice.
func uniqueStrings(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || s[i-1] != v {
			out = append(out, v)
		}
	}
	return out
}

// maxSiblingModules bounds how many uncovered modules are listed to find importers of an edited one.
const maxSiblingModules = 20

// Nested describes a module below the workspace root that the workspace's build does not cover.
type Nested struct {
	Path string // module path from its go.mod ("" if unreadable)
	Dir  string // directory holding its go.mod
}

// modulePathOf reads the module line of a go.mod.
func modulePathOf(gomod string) string {
	b, err := os.ReadFile(gomod)
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "module" {
			return strings.Trim(f[1], `"`)
		}
	}
	return ""
}

// Owner finds the module that owns dir: the nearest go.mod in dir or above it. Unlike `go list -m`,
// it does not depend on the go.work in effect, where that command lists every module.
func Owner(dir string) (Nested, bool) {
	for d := filepath.Clean(dir); ; {
		if p := modulePathOf(filepath.Join(d, "go.mod")); p != "" {
			return Nested{Path: p, Dir: d}, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return Nested{}, false
		}
		d = parent
	}
}

// findWork returns the go.work governing root (root or an ancestor; GOWORK overrides), "" if none.
func findWork(root string) string {
	switch env := os.Getenv("GOWORK"); {
	case env == "off":
		return ""
	case env != "":
		return env
	}
	for d := root; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.work")); err == nil {
			return filepath.Join(d, "go.work")
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// workUses lists the module directories of a go.work's use directives (absolute).
func workUses(work string) map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(work)
	if err != nil {
		return out
	}
	inBlock := false
	for _, l := range strings.Split(string(b), "\n") {
		if i := strings.Index(l, "//"); i >= 0 {
			l = l[:i]
		}
		f := strings.Fields(l)
		switch {
		case len(f) == 0:
			continue
		case inBlock && f[0] == ")":
			inBlock = false
			continue
		case !inBlock && f[0] == "use" && len(f) >= 2 && f[1] == "(":
			inBlock = true
			continue
		case !inBlock && f[0] == "use" && len(f) >= 2:
			f = f[1:]
		case !inBlock:
			continue
		}
		p := strings.Trim(f[0], `"`)
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(work), p)
		}
		out[filepath.Clean(p)] = true
	}
	return out
}

// Uncovered reports the module of file (its nearest go.mod at or below root) when the workspace
// does not build it: it is not the root's own module and no go.work lists it. Such a module's
// importers are not type-checked by the write tools, and gopls may not index it.
func Uncovered(root, file string) (Nested, bool) {
	root = filepath.Clean(root)
	modDir := ""
	for d := filepath.Dir(file); ; d = filepath.Dir(d) {
		if rel, err := filepath.Rel(root, d); err != nil || strings.HasPrefix(rel, "..") {
			break
		}
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			modDir = d
			break
		}
		if d == root || filepath.Dir(d) == d {
			break
		}
	}
	if modDir == "" || modDir == root {
		return Nested{}, false
	}
	if work := findWork(root); work != "" && workUses(work)[modDir] {
		return Nested{}, false
	}
	return Nested{Path: modulePathOf(filepath.Join(modDir, "go.mod")), Dir: modDir}, true
}

// UncoveredModules lists the modules below root (go.mod files in subdirectories) that the workspace
// does not build, at most limit of them.
func UncoveredModules(root string, skipDir func(name string) bool, limit int) []Nested {
	var out []Nested
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if len(out) >= limit {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if p != root && (skipDir != nil && skipDir(d.Name()) || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "go.mod" && filepath.Dir(p) != root {
			if n, ok := Uncovered(root, filepath.Join(filepath.Dir(p), "x.go")); ok {
				out = append(out, n)
			}
		}
		return nil
	})
	return out
}

// ConfigFiles lists the module files gopls loads the workspace of root from that may lie outside root: the
// go.mod and go.sum of each directory above it, and the go.work that governs it with its go.sum and the
// go.mod and go.sum of each module it uses. Paths may repeat and may not exist.
func ConfigFiles(root string) []string {
	root = filepath.Clean(root)
	var out []string
	for d := root; ; d = filepath.Dir(d) {
		out = append(out, filepath.Join(d, "go.mod"), filepath.Join(d, "go.sum"))
		if filepath.Dir(d) == d {
			break
		}
	}
	if work := findWork(root); work != "" {
		out = append(out, work, work+".sum")
		for d := range workUses(work) {
			out = append(out, filepath.Join(d, "go.mod"), filepath.Join(d, "go.sum"))
		}
	}
	return out
}
