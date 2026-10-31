package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

type moduleInfo struct{ Path, Dir string }

func moduleOf(dir string) (moduleInfo, error) {
	m, ok := gomod.Owner(dir)
	if !ok {
		return moduleInfo{}, toolerr.Inputf("%s is not inside a Go module", dir)
	}
	return moduleInfo{Path: m.Path, Dir: m.Dir}, nil
}

func importPathOf(m moduleInfo, dir string) (string, error) {
	rel, err := filepath.Rel(m.Dir, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", toolerr.Inputf("%s is outside the module %s", dir, m.Path)
	}
	if rel == "." {
		return m.Path, nil
	}
	return m.Path + "/" + filepath.ToSlash(rel), nil
}

// moveDirectory moves a package directory (with the packages below it) and rewrites every import of it.
func moveDirectory(ctx context.Context, w *Workspace, from, to string, in moveIn) (string, error) {
	src, err := w.absPath(from)
	if err != nil {
		return "", err
	}
	dst, err := w.absPath(to)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		return "", toolerr.Inputf("%s is not a directory.", from)
	}
	if src == w.Root {
		return "", toolerr.Inputf("the workspace root cannot be moved.")
	}
	if _, err := os.Stat(dst); err == nil {
		return "", toolerr.Inputf("%s already exists.", to)
	}
	if rel, err := filepath.Rel(src, dst); err == nil && !strings.HasPrefix(rel, "..") {
		return "", toolerr.Inputf("%s is inside %s.", to, from)
	}
	mod, err := moduleOf(src)
	if err != nil {
		return "", err
	}
	oldPath, err := importPathOf(mod, src)
	if err != nil {
		return "", err
	}
	newPath, err := importPathOf(mod, dst)
	if err != nil {
		return "", err
	}

	var moved []string
	var others []string
	_ = filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if strings.HasSuffix(p, ".go") {
				moved = append(moved, p)
			} else {
				others = append(others, edits.RelativeName(p, w.Root))
			}
		}
		return nil
	})
	if len(others) > 0 {
		sort.Strings(others)
		return "", toolerr.Inputf("%s contains files that are not Go source (%s); this tool moves .go files only. Move the directory with git mv and run verify_changes.",
			from, strings.Join(others[:min(len(others), 6)], ", "))
	}
	if len(moved) == 0 {
		return "", toolerr.Inputf("%s has no Go files.", from)
	}

	rewrite := func(p string) (string, bool) {
		switch {
		case p == oldPath:
			return newPath, true
		case strings.HasPrefix(p, oldPath+"/"):
			return newPath + p[len(oldPath):], true
		}
		return "", false
	}
	var changes []edits.FileChange
	imports := 0
	isMoved := map[string]bool{}
	for _, p := range moved {
		isMoved[p] = true
		text, bom, err := edits.ReadSource(p)
		if err != nil {
			return "", err
		}
		next, n, err := gosrc.RewriteImports(p, text, rewrite)
		if err != nil {
			return "", err
		}
		if n {
			imports++
		}
		old := text
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		changes = append(changes,
			edits.FileChange{Path: p, Old: &old, OldBOM: bom},
			edits.FileChange{Path: target, New: &next, NewBOM: bom})
	}
	_ = filepath.WalkDir(w.Root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != w.Root && (exclude.IsDirName(d.Name()) || isGitCheckout(p)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || isMoved[p] {
			return nil
		}
		text, bom, rerr := edits.ReadSource(p)
		if rerr != nil || !strings.Contains(text, oldPath) {
			return nil
		}
		next, changed, rerr := gosrc.RewriteImports(p, text, rewrite)
		if rerr != nil || !changed {
			return nil
		}
		c, rerr := fileChange(p, next, &text, bom)
		if rerr == nil && !c.IsNoop() {
			changes = append(changes, c)
			imports++
		}
		return nil
	})
	notes := []string{fmt.Sprintf("Import path %s -> %s rewritten in %d file(s); %d file(s) moved.", oldPath, newPath, imports, len(moved))}
	if filepath.Base(src) != filepath.Base(dst) {
		notes = append(notes, "The package clause was not renamed; callers keep using the old package name (rename it with edit if you want the new directory name).")
	}
	plan := edits.NewPlan("move directory "+from+" to "+to, changes, notes...)
	return finishPlan(ctx, w, plan, flag(in.Apply, true), limit(in.MaxNewErrors), flag(in.IncludeDependents, true), in.AllowLarge)
}

func isGitCheckout(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}
