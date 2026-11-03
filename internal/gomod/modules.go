package gomod

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Package is a package as `go list` sees it: its import path, package name, directory and Go files
// (absolute, not tests; build constraints already applied) and the imports of those files.
type Package struct {
	ImportPath string
	Name       string
	Dir        string
	Files      []string
	Imports    []string
}

func toPackage(p pkg) Package {
	var files []string
	for _, group := range [][]string{p.GoFiles, p.CgoFiles} {
		for _, f := range group {
			files = append(files, filepath.Join(p.Dir, f))
		}
	}
	return Package{ImportPath: p.ImportPath, Name: p.Name, Dir: p.Dir, Files: files, Imports: p.Imports}
}

// Packages lists the packages of the workspace below root (the same listing as Related's, cached).
func Packages(ctx context.Context, root string) ([]Package, error) {
	pkgs, err := list(ctx, root)
	if err != nil && len(pkgs) == 0 {
		return nil, err
	}
	var out []Package
	for _, p := range pkgs {
		if p.ImportPath != "" {
			out = append(out, toPackage(p))
		}
	}
	return out, nil
}

// Resolve lists the packages with the given import paths as seen from dir (any package the module graph of dir
// provides, the standard library included). Paths that do not resolve are left out.
func Resolve(ctx context.Context, dir string, paths []string) []Package {
	if len(paths) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	args := append([]string{"list", "-e", "-json=ImportPath,Name,Dir,GoFiles,CgoFiles,Imports"}, paths...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	_ = cmd.Run()
	var res []Package
	dec := json.NewDecoder(&out)
	for dec.More() {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			break
		}
		if p.ImportPath != "" && p.Dir != "" {
			res = append(res, toPackage(p))
		}
	}
	return res
}

// Module is a module of a build list: its path and the directory holding its source ("" if not downloaded).
type Module struct {
	Path string
	Dir  string
}

type buildEntry struct {
	key  string
	at   time.Time
	mods []Module
}

var buildCache = struct {
	sync.Mutex
	byDir map[string]buildEntry
}{byDir: map[string]buildEntry{}}

// statKey summarizes a file's identity for a cache key ("" if it does not exist).
func statKey(p string) string {
	st, err := os.Stat(p)
	if err != nil {
		return ""
	}
	return p + ":" + strconv.FormatInt(st.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(st.Size(), 10)
}

// BuildList lists the modules of the build list of the module (or workspace) that governs dir: `go list -m -json
// all` run in dir. ok is false when go lists no module at all, so the caller cannot tell what is importable; a module
// that cannot be read is listed with its path (parseBuildList), and the others still count. The result
// is cached while the governing go.mod, go.sum and go.work files are unchanged.
func BuildList(ctx context.Context, dir string) (mods []Module, ok bool) {
	modDir := dir
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			modDir = d
			break
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	key := modDir + "|" + os.Getenv("GOWORK") + "|" + os.Getenv("GOFLAGS") + "|" + statKey(filepath.Join(modDir, "go.mod")) +
		"|" + statKey(filepath.Join(modDir, "go.sum")) + "|" + statKey(findWork(modDir))
	buildCache.Lock()
	if e, hit := buildCache.byDir[dir]; hit && e.key == key && time.Since(e.at) < listTTL {
		buildCache.Unlock()
		return e.mods, true
	}
	buildCache.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// -e: a module whose go.mod cannot be read (offline, not in the module cache) is listed with an error and the
	// rest of the build list still is, so the exit status does not decide; the listing does.
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-m", "-json", "all")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	_ = cmd.Run()
	mods, ok = parseBuildList(&out)
	if !ok {
		return nil, false
	}
	buildCache.Lock()
	buildCache.byDir[dir] = buildEntry{key, time.Now(), mods}
	buildCache.Unlock()
	return mods, true
}

// parseBuildList reads the modules of a `go list -m -json` listing (concatenated objects). ok is false when nothing
// is listed or the output is not that listing, so the caller cannot tell what is importable.
func parseBuildList(r io.Reader) (mods []Module, ok bool) {
	dec := json.NewDecoder(r)
	for dec.More() {
		var m Module
		if err := dec.Decode(&m); err != nil {
			return nil, false
		}
		mods = append(mods, m)
	}
	return mods, len(mods) > 0
}
