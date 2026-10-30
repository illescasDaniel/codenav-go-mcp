// Package gomod answers package-graph questions through `go list`.
package gomod

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type pkg struct {
	ImportPath   string
	Dir          string
	GoFiles      []string
	CgoFiles     []string
	TestGoFiles  []string
	XTestGoFiles []string
	Imports      []string
	TestImports  []string
	XTestImports []string
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

// List returns every package of the module(s) under root.
func list(ctx context.Context, root string) ([]pkg, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-json=ImportPath,Dir,GoFiles,CgoFiles,TestGoFiles,XTestGoFiles,Imports,TestImports,XTestImports", "./...")
	cmd.Dir = root
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil && out.Len() == 0 {
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
func Related(ctx context.Context, root string, edited []string) (files []string, err error) {
	pkgs, err := list(ctx, root)
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
	if err != nil && len(pkgs) == 0 {
		return files, err
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
		}
	}
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
	return files, nil
}

// IsExcludedFromBuild is a best-effort hint that a file has a build constraint; true when its
// "//go:build" line is present. gopls only checks the current GOOS/GOARCH/tags.
func HasBuildConstraint(src string) bool {
	for _, l := range strings.SplitN(src, "\n", 40) {
		if strings.HasPrefix(l, "//go:build ") || strings.HasPrefix(l, "// +build ") {
			return true
		}
		if strings.HasPrefix(l, "package ") {
			return false
		}
	}
	return false
}

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
