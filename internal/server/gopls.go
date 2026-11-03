package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GoplsCommand locates gopls: $CODENAV_GO_MCP_GOPLS, then GOBIN / GOPATH/bin, then PATH,
// and as a last resort `go run golang.org/x/tools/gopls@latest` (with a warning).
func GoplsCommand() (cmd []string, warning string, err error) {
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
	for _, dir := range goBinDirs() {
		p := filepath.Join(dir, "gopls")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
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

func goBinDirs() []string {
	var dirs []string
	if v := os.Getenv("GOBIN"); v != "" {
		dirs = append(dirs, v)
	}
	if v := os.Getenv("GOPATH"); v != "" {
		for _, p := range filepath.SplitList(v) {
			dirs = append(dirs, filepath.Join(p, "bin"))
		}
	} else if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "go", "bin"))
	}
	return dirs
}
