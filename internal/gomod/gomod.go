// Package gomod answers package-graph questions through `go list`.
package gomod

import (
	"bytes"
	"context"
	"encoding/json"
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
