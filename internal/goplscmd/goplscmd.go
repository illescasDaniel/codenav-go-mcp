// Package goplscmd finds the gopls command the server runs. The server and the workspace tool both ask it, so
// that what codenav reports as the gopls in use is the binary it starts.
package goplscmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EnvGopls names the variable that pins the gopls command.
const EnvGopls = "CODENAV_GO_MCP_GOPLS"

// Command locates gopls: $CODENAV_GO_MCP_GOPLS, then GOBIN / GOPATH/bin (each GOPATH entry), then PATH,
// and as a last resort `go run golang.org/x/tools/gopls@latest` (with a warning). A pinned value that is not
// usable is an error; it is not replaced by a search.
func Command() (cmd []string, warning string, err error) {
	if p := os.Getenv(EnvGopls); strings.TrimSpace(p) != "" {
		// The whole value is the path when it is an executable file (a directory may hold a space); otherwise
		// it is split on whitespace so that `gopls -remote=...` style values still work.
		fields := []string{p}
		if !isExecutableFile(p) {
			fields = strings.Fields(p)
		}
		// Not wrapping the lookup error keeps it out of the "File not found" rewording of the tool errors.
		if _, err := exec.LookPath(fields[0]); err != nil {
			return nil, "", fmt.Errorf("gopls not found at %s ($%s)", p, EnvGopls)
		}
		return fields, "", nil
	}
	for _, dir := range binDirs() {
		p := filepath.Join(dir, "gopls")
		if isExecutableFile(p) {
			return []string{p}, "", nil
		}
	}
	if p, err := exec.LookPath("gopls"); err == nil {
		return []string{p}, "", nil
	}
	if goTool, err := exec.LookPath("go"); err == nil {
		return []string{goTool, "run", "golang.org/x/tools/gopls@latest"},
			"gopls was not found; falling back to `go run golang.org/x/tools/gopls@latest` (slow on first use). Install it with `go install golang.org/x/tools/gopls@latest`.", nil
	}
	return nil, "", fmt.Errorf("gopls not found and no go toolchain on PATH; install it with `go install golang.org/x/tools/gopls@latest` or set $%s", EnvGopls)
}

// isExecutableFile reports whether p names an existing regular file with an execute bit (symlinks followed).
func isExecutableFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

// binDirs are the directories gopls is looked for in before PATH: a non-empty GOBIN, then the bin directory of each
// GOPATH entry (~/go/bin when GOPATH is unset).
func binDirs() []string {
	var dirs []string
	if v := os.Getenv("GOBIN"); v != "" {
		dirs = append(dirs, v)
	}
	if v := os.Getenv("GOPATH"); v != "" {
		for _, p := range filepath.SplitList(v) {
			if p != "" {
				dirs = append(dirs, filepath.Join(p, "bin"))
			}
		}
	} else if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "go", "bin"))
	}
	return dirs
}
