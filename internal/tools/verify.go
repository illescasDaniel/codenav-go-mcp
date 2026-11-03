package tools

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/illescasDaniel/codenav-go-mcp/internal/diagdelta"
	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

func git(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	out, err := cmd.Output()
	return string(out), err
}

func verifyChanges(ctx context.Context, w *Workspace, includeDeps bool) (string, error) {
	// --no-renames: a renamed file shows as its old path (deleted) and its new path (added), so the before-run
	// still has the old declarations.
	changed, err := git(ctx, w.Root, "diff", "--name-only", "-z", "--relative", "--no-renames", "HEAD")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", toolerr.Inputf("cannot compare with git HEAD: the git command was not found on PATH.")
		}
		return "", toolerr.Inputf("cannot compare with git HEAD (not a git repository, or no commits yet).")
	}
	// Without --full-name the paths are relative to the workspace, like the diff above (--full-name is relative to
	// the git top, which is wrong when the module is below it).
	untracked, _ := git(ctx, w.Root, "ls-files", "--others", "--exclude-standard", "-z")
	before := map[string]string{}
	skipBefore, skipAfter := map[string]bool{}, map[string]bool{}
	var edited, moduleFiles, deleted []string
	seen := map[string]bool{}
	// A file absent at HEAD must not keep its declarations in the before-run (gopls reads the disk): overlay its
	// package clause only, as a deleted file does in Simulate. Its own diagnostics are skipped on that side.
	absentAtHEAD := func(abs string) {
		skipBefore[abs] = true
		if b, err := os.ReadFile(abs); err == nil {
			if stub := packageStub(string(b)); stub != "" {
				before[abs] = stub
			}
		}
	}
	add := func(rel string, isNew bool) {
		if rel != "" && filepath.Ext(rel) != ".go" && isModuleFile(rel) && !seen[rel] {
			// A module file is not type-checked, but the Go configuration it sets is: say it changed.
			seen[rel] = true
			moduleFiles = append(moduleFiles, rel)
		}
		if rel == "" || filepath.Ext(rel) != ".go" || seen[rel] {
			return
		}
		seen[rel] = true
		abs := filepath.Join(w.Root, rel)
		edited = append(edited, abs)
		if _, err := os.Stat(abs); errors.Is(err, fs.ErrNotExist) {
			deleted = append(deleted, rel)
		}
		if isNew {
			absentAtHEAD(abs)
			return
		}
		if src, err := git(ctx, w.Root, "show", "HEAD:./"+rel); err == nil {
			before[abs] = src
		} else {
			absentAtHEAD(abs)
		}
	}
	for _, f := range strings.Split(changed, "\x00") {
		add(f, false)
	}
	for _, f := range strings.Split(untracked, "\x00") {
		add(f, true)
	}
	if len(edited) == 0 {
		if len(moduleFiles) > 0 {
			return "Uncommitted changes vs HEAD in module file(s) only: " + strings.Join(moduleFiles, ", ") +
				". No Go file changed, so no Go file was type-checked: the Go configuration was reloaded, and run go build to confirm the build.", nil
		}
		return "No uncommitted Go changes.", nil
	}
	// Deleted files: absent on disk now.
	files, left, incomplete := FilesToCheck(ctx, w, edited, includeDeps)
	d, err := Delta(ctx, w, before, nil, files, skipBefore, skipAfter)
	if err != nil {
		return "", err
	}
	d.NotChecked = gateReasons(gomod.Health(ctx, w.Root), incomplete, d.FilesChecked)
	d.Capped = len(left)
	d.DependentsOff = !includeDeps
	out := "Uncommitted changes vs HEAD in " + strconv.Itoa(len(edited)) + " Go file(s).\n" + diagdelta.Format(d, 15)
	if len(moduleFiles) > 0 {
		out += "\nModule file(s) changed too: " + strings.Join(moduleFiles, ", ") + " (a broken Go configuration is reported in the warning, if any)."
	}
	if len(deleted) > 0 {
		// A deleted file has no diagnostics of its own: name it, or "N Go file(s)" is unexplained.
		out += "\nDeleted Go file(s): " + strings.Join(deleted, ", ") + "."
	}
	if len(left) > 0 {
		out += "\n(" + leftOutNote(w, left, edited) + ")"
	}
	return out, nil
}

// isModuleFile reports whether rel is a module file that sets the Go configuration of the workspace.
func isModuleFile(rel string) bool {
	switch filepath.Base(rel) {
	case "go.mod", "go.sum", "go.work", "go.work.sum":
		return true
	}
	return false
}
