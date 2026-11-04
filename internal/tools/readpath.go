package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/modfile"

	"github.com/illescasDaniel/codenav-go-mcp/internal/toolerr"
)

var readRoots sync.Map // workspace root -> []string

// readableRoots are the directories the read tools may look into besides the workspace: the Go
// root, the module cache and the directories that go.mod / go.work replace directives point at.
func readableRoots(root string) []string {
	if v, ok := readRoots.Load(root); ok {
		return v.([]string)
	}
	var dirs []string
	for _, name := range []string{"GOROOT", "GOMODCACHE"} {
		if out, err := exec.Command("go", "env", name).Output(); err == nil {
			if d := strings.TrimSpace(string(out)); d != "" {
				dirs = append(dirs, d)
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		if f, err := modfile.Parse("go.mod", data, nil); err == nil {
			for _, r := range f.Replace {
				dirs = append(dirs, replaceDir(root, r.New.Path))
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(root, "go.work")); err == nil {
		if f, err := modfile.ParseWork("go.work", data, nil); err == nil {
			for _, r := range f.Replace {
				dirs = append(dirs, replaceDir(root, r.New.Path))
			}
			for _, u := range f.Use {
				dirs = append(dirs, replaceDir(root, u.Path))
			}
		}
	}
	readRoots.Store(root, dirs)
	return dirs
}

// replaceDir resolves a replace target; module paths (not directories) give "".
func replaceDir(root, p string) string {
	if p == "" || !(filepath.IsAbs(p) || strings.HasPrefix(p, ".")) {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	return p
}

var goRootDir struct {
	once sync.Once
	dir  string
}

// goEnvTimeout bounds the `go env GOROOT` that stdlibTop runs; a hung go command must not hang a hover.
var goEnvTimeout = 5 * time.Second

// stdlibTop reports whether first is a top-level directory of the Go root's src tree, that is, a standard
// library package path. When the Go root is unknown (go env fails or does not answer in goEnvTimeout) it answers
// true (keeps the link).
//
// The Go root is looked up once per process. A timed-out `go env GOROOT` is cached as unknown for the process
// lifetime, so stdlib detection stays off until restart; the fallback keeps the link, so only the filtering is lost.
func stdlibTop(first string) bool {
	goRootDir.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), goEnvTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "env", "GOROOT")
		cmd.WaitDelay = time.Second // a child of go that keeps the output pipe open must not hold the call
		if out, err := cmd.Output(); err == nil {
			goRootDir.dir = strings.TrimSpace(string(out))
		}
	})
	if goRootDir.dir == "" {
		return true
	}
	_, err := os.Stat(filepath.Join(goRootDir.dir, "src", first))
	return err == nil
}

func realPath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func under(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// checkReadable refuses a file_path that points outside every place the read tools may look.
func (w *Workspace) checkReadable(p string) error {
	if p == "" {
		return nil
	}
	given := p
	if !filepath.IsAbs(p) {
		p = filepath.Join(w.Root, p)
	}
	p = realPath(p)
	if under(realPath(w.Root), p) {
		return nil
	}
	for _, d := range readableRoots(w.Root) {
		if d != "" && under(realPath(d), p) {
			return nil
		}
	}
	return toolerr.Inputf("%s is outside the workspace, the Go root, the module cache and the replaced modules; only files in those places can be read.", given)
}

// filePathOf returns the "file_path" field of a tool's input ("" when it has none).
func filePathOf(in any) string {
	b, err := json.Marshal(in)
	if err != nil {
		return ""
	}
	var f struct {
		FilePath string `json:"file_path"`
	}
	_ = json.Unmarshal(b, &f)
	return f.FilePath
}

// scrub shows the workspace-relative form of the paths in a message: parser, gopls and tool messages
// carry absolute file names. Only a path that starts at a word boundary is rewritten, so
// "/other/<root>/x" stays as it is.
func (w *Workspace) scrub(msg string) string {
	if w.Root == "" {
		return msg
	}
	roots := []string{strings.TrimRight(w.Root, "/")}
	if real, err := filepath.EvalSymlinks(w.Root); err == nil && strings.TrimRight(real, "/") != roots[0] {
		roots = append(roots, strings.TrimRight(real, "/"))
	}
	for _, r := range roots {
		msg = stripPathPrefix(msg, r+"/")
	}
	return msg
}

// stripPathPrefix removes prefix wherever it starts a path (not preceded by a path character).
func stripPathPrefix(msg, prefix string) string {
	var sb strings.Builder
	for {
		i := strings.Index(msg, prefix)
		if i < 0 {
			break
		}
		sb.WriteString(msg[:i])
		if i > 0 && strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-/~", rune(msg[i-1])) {
			sb.WriteString(prefix)
		}
		msg = msg[i+len(prefix):]
	}
	sb.WriteString(msg)
	return sb.String()
}
