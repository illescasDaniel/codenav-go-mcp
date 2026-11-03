package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/edits"
	"github.com/illescasDaniel/codenav-go-mcp/internal/exclude"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gosrc"
	"github.com/illescasDaniel/codenav-go-mcp/internal/mentions"
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
	src, err := w.argPath(from)
	if err != nil {
		return "", err
	}
	dst, err := w.argPath(to)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(src); err == nil && !st.IsDir() {
		return "", toolerr.Inputf("%s is a file, and file_path + to_file without name moves a whole directory (a package). To move a symbol out of it pass name=<symbol> with to_file=<.go file>; moving a single file is not supported.", from)
	} else if err != nil || !st.IsDir() {
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
	// The destination may sit in another module (a nested go.mod): its import path comes from there.
	dstMod, err := moduleOf(dst)
	if err != nil {
		return "", err
	}
	newPath, err := importPathOf(dstMod, dst)
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
	// after and renamed describe the files as the plan leaves them, for the leftover scan (BUG-MOVE-003).
	after := map[string]string{}
	renamed := map[string]string{}
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
		after[p], renamed[p] = next, target
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
		after[p] = next
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
	if dstMod.Dir != mod.Dir {
		notes = append(notes, fmt.Sprintf("Moved to another module (%s -> %s): packages of %s resolve %s only through a go.work that joins both modules, or a require/replace in their go.mod.", mod.Path, dstMod.Path, mod.Path, newPath))
	}
	notes = append(notes, staleImportNotes(w, oldPath, newPath, after, renamed)...)
	plan := edits.NewPlan("move directory "+from+" to "+to, changes, notes...)
	return finishPlan(ctx, w, plan, flag(in.Apply, true), limit(in.MaxNewErrors), flag(in.IncludeDependents, true), in.AllowLarge)
}

func isGitCheckout(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// scannedByMentions are the extensions mentions.FindScoped reads (keep in step with its textSuffixes).
var scannedByMentions = map[string]bool{
	".go": true, ".md": true, ".rst": true, ".txt": true, ".toml": true, ".yml": true, ".yaml": true,
	".json": true, ".cfg": true, ".ini": true,
}

// staleMaxBytes is the largest file the leftover scan reads (as mentions does).
const staleMaxBytes = 512_000

// staleMaxFiles caps the files the plain-text leftover scan reads (as mentions caps its own scan).
const staleMaxFiles = 2000

type staleMention struct {
	path    string
	line    int
	snippet string
}

// staleImportNotes lists the places that still name oldPath after a package move, where the import rewrite does not
// reach: a //go:generate line, a Makefile, a Dockerfile, a goreleaser flag, testdata (BUG-MOVE-003). after maps a file
// on disk to the text it has once the plan is applied; renamed maps the path of a moved file to its new path. newPath
// is taken out of each line before it is matched, so a rewrite of x/lib to x/lib-v2 leaves nothing stale.
func staleImportNotes(w *Workspace, oldPath, newPath string, after, renamed map[string]string) []string {
	moved := map[string]string{}
	for from, to := range renamed {
		moved[edits.RelativeName(from, w.Root)] = edits.RelativeName(to, w.Root)
	}
	word := staleWord(oldPath)
	found, truncated := mentions.FindScopedLimited(w.Root, oldPath, after, "", nil)
	lines := map[string][]string{} // the files found above, split into lines as the plan leaves them
	var hits []staleMention
	for _, m := range found {
		if !staleLine(word, lineAt(w.Root, after, lines, m.Path, m.Line), newPath) {
			continue
		}
		path := m.Path
		if to, ok := moved[path]; ok {
			path = to
		}
		hits = append(hits, staleMention{path, m.Line, m.Snippet})
	}
	plain, plainTruncated := plainTextMentions(w, oldPath, newPath, word)
	hits = append(hits, plain...)
	var notes []string
	if len(hits) > 0 {
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].path != hits[j].path {
				return hits[i].path < hits[j].path
			}
			return hits[i].line < hits[j].line
		})
		items := make([]string, 0, len(hits))
		for _, h := range hits {
			items = append(items, fmt.Sprintf("%s:%d (%s)", h.path, h.line, h.snippet))
		}
		notes = append(notes, fmt.Sprintf("%d place(s) still mention the old import path %s (not rewritten): %s.", len(items), oldPath, listing(items)))
	}
	if truncated || plainTruncated {
		notes = append(notes, fmt.Sprintf("The scan for places that still mention %s stopped early (more than %d files); the rest of the tree was not checked.", oldPath, staleMaxFiles))
	}
	return notes
}

// staleWord matches oldPath as a whole path: x/lib in "x/lib" or `x/lib`, not in x/lib2.
func staleWord(oldPath string) *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[^\w])` + regexp.QuoteMeta(oldPath) + `(?:[^\w]|$)`)
}

// staleLine reports whether line still names the old path once newPath is taken out of it: x/lib-v2 and x/lib.new
// are not x/lib.
func staleLine(word *regexp.Regexp, line, newPath string) bool {
	if newPath != "" {
		line = strings.ReplaceAll(line, newPath, "")
	}
	return word.MatchString(line)
}

// lineAt is line n (1-based) of the file rel (relative to root) as the plan leaves it: the text in after when the
// plan has one, else the file on disk. lines caches the files already split.
func lineAt(root string, after map[string]string, lines map[string][]string, rel string, n int) string {
	ls, ok := lines[rel]
	if !ok {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		text, planned := after[abs]
		if !planned {
			if raw, err := os.ReadFile(abs); err == nil {
				text, _ = edits.DecodeSource(raw)
			}
		}
		ls = strings.Split(text, "\n")
		lines[rel] = ls
	}
	if n < 1 || n > len(ls) {
		return ""
	}
	return ls[n-1]
}

// plainTextMentions finds oldPath in the files mentions.FindScoped does not read (a Makefile, a Dockerfile and other
// files without a text extension). Only regular files are opened: a FIFO, a device or a symlink could block the scan
// or be read without end. Hidden and excluded directories, binary files and files over staleMaxBytes are skipped.
// truncated reports that the scan stopped at staleMaxFiles files.
func plainTextMentions(w *Workspace, oldPath, newPath string, word *regexp.Regexp) (out []staleMention, truncated bool) {
	files := 0
	_ = filepath.WalkDir(w.Root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != w.Root && (strings.HasPrefix(d.Name(), ".") || exclude.IsDirName(d.Name()) || isGitCheckout(p)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || scannedByMentions[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		if info, ierr := d.Info(); ierr != nil || info.Size() > staleMaxBytes {
			return nil
		}
		if files++; files > staleMaxFiles {
			truncated = true
			return filepath.SkipAll
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil || bytes.IndexByte(raw, 0) >= 0 || !bytes.Contains(raw, []byte(oldPath)) {
			return nil
		}
		rel := edits.RelativeName(p, w.Root)
		for i, line := range strings.Split(string(raw), "\n") {
			if staleLine(word, line, newPath) {
				snip := strings.TrimSpace(line)
				if len(snip) > 140 {
					snip = snip[:140]
				}
				out = append(out, staleMention{rel, i + 1, snip})
			}
		}
		return nil
	})
	return out, truncated
}
