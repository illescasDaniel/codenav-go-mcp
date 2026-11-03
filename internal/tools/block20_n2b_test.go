package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/illescasDaniel/codenav-go-mcp/internal/lsp"
)

// execScript writes an executable shell script at dir/name and returns its path.
func execScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// onlyPath points PATH at an empty directory and clears the gopls variables, so the search finds nothing unless the
// test provides it.
func onlyPath(t *testing.T) {
	t.Helper()
	t.Setenv("CODENAV_GO_MCP_GOPLS", "")
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", t.TempDir())
	t.Setenv("PATH", t.TempDir())
}

// BUG-LEAK-005 / ENV-007 (gopls part): the gopls line is the command the server runs, with its arguments, and its
// version is read by running that command with `version`.
func TestBlock20NWorkspaceGoplsIsTheServersCommand(t *testing.T) {
	onlyPath(t)
	bin := execScript(t, t.TempDir(), "gopls", `echo "golang.org/x/tools/gopls v9.9.9-n2 args:$*"`)
	t.Setenv("CODENAV_GO_MCP_GOPLS", bin+" -remote=auto")
	out := workspaceReport(context.Background(), fakeHost{}, &Workspace{Root: t.TempDir()})
	if !strings.Contains(out, "gopls: "+bin+" -remote=auto (golang.org/x/tools/gopls v9.9.9-n2 args:-remote=auto version)") {
		t.Errorf("pinned gopls line:\n%s", out)
	}
}

// An unusable pin is said so, with the variable; it is not reported as a search that found nothing.
func TestBlock20NWorkspaceUnusablePinSaidSo(t *testing.T) {
	onlyPath(t)
	t.Setenv("CODENAV_GO_MCP_GOPLS", filepath.Join(t.TempDir(), "missing", "gopls"))
	out := workspaceReport(context.Background(), fakeHost{}, &Workspace{Root: t.TempDir()})
	if !strings.Contains(out, "gopls: not usable: gopls not found at") || !strings.Contains(out, "CODENAV_GO_MCP_GOPLS") {
		t.Errorf("unusable pin:\n%s", out)
	}
}

// With only go on PATH, the fallback is named as a fallback and its version is not fetched.
func TestBlock20NWorkspaceGoRunFallbackNamed(t *testing.T) {
	onlyPath(t)
	dir := t.TempDir()
	execScript(t, dir, "go", "exit 0")
	t.Setenv("PATH", dir)
	out := workspaceReport(context.Background(), fakeHost{}, &Workspace{Root: t.TempDir()})
	if !strings.Contains(out, "run golang.org/x/tools/gopls@latest (fallback: gopls is not installed") {
		t.Errorf("go run fallback:\n%s", out)
	}
}

// BUG-LEAK-005 (secrets): the -X values of -ldflags are not printed; the flag names are.
func TestBlock20NRedactFlagsHidesLdflagsValues(t *testing.T) {
	cases := map[string]struct{ want, not []string }{
		"-ldflags=-X main.token=s3cret -tags x": {want: []string{"-ldflags=<redacted>", "-tags x"}, not: []string{"s3cret", "main.token"}},
		"-ldflags -X main.a=hunter2 -race":      {want: []string{"-ldflags=<redacted>", "-race"}, not: []string{"hunter2", "main.a"}},
		"-X main.k=v2 -count=1":                 {want: []string{"-X=<redacted>", "-count=1"}, not: []string{"v2", "main.k"}},
	}
	for in, c := range cases {
		got := redactFlags(in)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("redactFlags(%q) = %q, lacks %q", in, got, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Errorf("redactFlags(%q) = %q, still has %q", in, got, n)
			}
		}
	}
}

// The GOFLAGS line of the workspace report is redacted too.
func TestBlock20NWorkspaceGoflagsRedacted(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{"go.mod": "module example.com/n2f\n\ngo 1.21\n"})
	t.Setenv("GOFLAGS", "-ldflags=-X main.token=s3cret")
	out := workspaceReport(context.Background(), fakeHost{}, &Workspace{Root: root})
	if !strings.Contains(out, "GOFLAGS: -ldflags=<redacted>") || strings.Contains(out, "s3cret") {
		t.Errorf("GOFLAGS not redacted:\n%s", out)
	}
}

// BUG-NAVN-012 (package and vendor): a type is qualified by its package clause, a vendor file is a dependency, and
// the other modules of a go.work are the workspace's own.
func TestBlock20NImplementerGroupsByPackageVendorAndWorkModules(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{
		"go.mod":            "module example.com/n2g\n\ngo 1.21\n",
		"go.work":           "go 1.21\n\nuse (\n\t.\n\t./sib\n)\n",
		"sib/go.mod":        "module example.com/n2g/sib\n\ngo 1.21\n",
		"sib/s.go":          "package sib\n\ntype Sib struct{}\n\nfunc (Sib) M() {}\n",
		"vendor/v/v.go":     "package vv\n\ntype Vend interface{ V() }\n",
		"x/unused_iface.go": "package x\n",
	})
	sib := filepath.Join(root, "sib", "s.go")
	vend := filepath.Join(root, "vendor", "v", "v.go")
	sibText := "package sib\n\ntype Sib struct{}\n\nfunc (Sib) M() {}\n"
	vendText := "package vv\n\ntype Vend interface{ V() }\n"
	locs := []lsp.Location{testLoc(sib, sibText, "Sib struct"), testLoc(vend, vendText, "Vend interface")}
	out := implementerGroups(locs, root, toolchainDirs{})
	for _, want := range []string{"Types (1): Sib", "Dependencies (1): vv.Vend"} {
		if !strings.Contains(out, want) {
			t.Errorf("grouped implementers lack %q:\n%s", want, out)
		}
	}
}

// BUG-NAVP-003 (wording and the all-dropped case): when every reference is a dependency's, the answer says so and
// does not say "No references found"; when some are the workspace's, the count of the others follows them.
func TestBlock20NReferencesAnswerAllDroppedAndMixed(t *testing.T) {
	root := t.TempDir()
	modcache := t.TempDir()
	b20dTree(t, root, map[string]string{"use.go": "package use\n\nfunc U() {}\n"})
	dep := filepath.Join(modcache, "github.com", "x@v1", "assert", "a.go")
	dirs := toolchainDirs{GOMODCACHE: modcache}
	depLocs := []lsp.Location{
		{URI: "file://" + filepath.ToSlash(dep)},
		{URI: "file://" + filepath.ToSlash(dep)},
	}
	all := referencesAnswer(depLocs, root, dirs)
	if !strings.Contains(all, "only 2 reference(s), all inside dependencies") || strings.Contains(all, "No references found") {
		t.Errorf("all dependency references:\n%s", all)
	}
	mixed := referencesAnswer(append([]lsp.Location{{URI: "file://" + filepath.ToSlash(filepath.Join(root, "use.go"))}}, depLocs[0]), root, dirs)
	if !strings.Contains(mixed, "use.go") || !strings.Contains(mixed, "(1 reference(s) inside dependencies not shown)") {
		t.Errorf("mixed references:\n%s", mixed)
	}
}

// BUG-NAVN-011 (budget): a scan whose deadline has passed stops and says it was truncated.
func TestBlock20NNestedScanBudgetTruncates(t *testing.T) {
	root := t.TempDir()
	b20dTree(t, root, map[string]string{"sub/go.mod": "module example.com/n2/sub\n\ngo 1.21\n", "sub/s.go": "package sub\n"})
	if res := scanNested(root, time.Now().Add(-time.Second)); !res.truncated {
		t.Errorf("a scan past its deadline was not reported as truncated")
	}
	if res := scanNested(root, time.Now().Add(time.Minute)); res.truncated || len(res.mods) != 1 {
		t.Errorf("a scan within budget: %+v", res)
	}
}

// BUG-NAVN-011 (limit): more nested modules than the limit is said, not silently missed.
func TestBlock20NNestedImplementersNoteSaysWhenScanIsCut(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	for _, m := range []string{"m1", "m2", "m3", "m4"} {
		files[m+"/go.mod"] = "module example.com/n2cut/" + m + "\n\ngo 1.21\n"
		files[m+"/x.go"] = "package " + m + "\n"
	}
	b20dTree(t, root, files)
	w := &Workspace{Root: root}
	if note := nestedImplementersNote(w, "Iface"); !strings.Contains(note, "stopped early") {
		t.Errorf("four nested modules, the limit is three; no note:\n%q", note)
	}
}

// BUG-LEAK-005 (go env retry): a failed `go env` is not kept; a later call asks again.
func TestBlock20NToolchainFailureNotCached(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	if d := toolchainLocs(missing); d.GOROOT != "" || d.GOMODCACHE != "" {
		t.Fatalf("unknown root gave %+v", d)
	}
	toolchain.Lock()
	_, kept := toolchain.byRoot[missing]
	toolchain.Unlock()
	if kept {
		t.Errorf("a failed go env was cached")
	}
}
