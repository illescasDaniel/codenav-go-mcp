package tools

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/diagdelta"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

func git(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	out, err := cmd.Output()
	return string(out), err
}

func verifyChanges(ctx context.Context, w *Workspace, includeDeps bool) (string, error) {
	changed, err := git(ctx, w.Root, "diff", "--name-only", "-z", "--relative", "HEAD")
	if err != nil {
		return "", toolerr.Inputf("cannot compare with git HEAD (not a git repository, or no commits yet).")
	}
	untracked, _ := git(ctx, w.Root, "ls-files", "--others", "--exclude-standard", "-z", "--full-name")
	before := map[string]string{}
	skipBefore, skipAfter := map[string]bool{}, map[string]bool{}
	var edited []string
	seen := map[string]bool{}
	add := func(rel string, isNew bool) {
		if rel == "" || filepath.Ext(rel) != ".go" || seen[rel] {
			return
		}
		seen[rel] = true
		abs := filepath.Join(w.Root, rel)
		edited = append(edited, abs)
		if isNew {
			skipBefore[abs] = true
			return
		}
		if src, err := git(ctx, w.Root, "show", "HEAD:./"+rel); err == nil {
			before[abs] = src
		} else {
			skipBefore[abs] = true
		}
	}
	for _, f := range strings.Split(changed, "\x00") {
		add(f, false)
	}
	for _, f := range strings.Split(untracked, "\x00") {
		add(f, true)
	}
	if len(edited) == 0 {
		return "No uncommitted Go changes.", nil
	}
	// Deleted files: absent on disk now.
	files, left := FilesToCheck(ctx, w, edited, includeDeps)
	d, err := Delta(ctx, w, before, nil, files, skipBefore, skipAfter)
	if err != nil {
		return "", err
	}
	out := "Uncommitted changes vs HEAD in " + strconv.Itoa(len(edited)) + " Go file(s).\n" + diagdelta.Format(d, 15)
	if len(left) > 0 {
		out += "\n(" + leftOutNote(w, left) + ")"
	}
	return out, nil
}
