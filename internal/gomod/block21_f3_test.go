package gomod

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// b21fWrite writes the files of a fixture below root (paths relative to root).
func b21fWrite(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, text := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// b21fOffline keeps the go command from the network and from a go.work above the fixture.
func b21fOffline(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go command is not on PATH")
	}
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
}

// BUG-LEAK-003: a replaced module whose go.mod does not parse is named as the cause, relative to the workspace root.
func TestBlock21F3_ImportCauseBrokenReplacedGoMod(t *testing.T) {
	b21fOffline(t)
	root := t.TempDir()
	b21fWrite(t, root, map[string]string{
		"go.mod":     "module example.com/leakrep\n\ngo 1.21\n\nrequire example.com/dep v0.0.0\n\nreplace example.com/dep => ./dep\n",
		"x/x.go":     "package x\n\nimport \"example.com/dep\"\n\nfunc X() int { return dep.Dep() }\n",
		"dep/go.mod": "module example.com/dep\n\ngo 1.21\n\nrequire (\n\tgolang.org/x/nothere v1.0.0\n",
		"dep/dep.go": "package dep\n\nfunc Dep() int { return 1 }\n",
	})
	got := ImportCause(context.Background(), root, filepath.Join(root, "x", "x.go"), "example.com/dep")
	if !strings.Contains(got, "parsing dep/go.mod") || !strings.Contains(got, "dep/go.mod:7") {
		t.Fatalf("cause = %q, want the parse error of dep/go.mod", got)
	}
	if strings.Contains(got, root) {
		t.Errorf("cause %q shows an absolute path of the workspace", got)
	}
	if len(strings.Split(got, "\n")) > 2 || len(got) > 400 {
		t.Errorf("cause is a dump: %q", got)
	}
}

// BUG-LEAK-003 (missing go.sum): a required module with no go.sum entry, while fetching is disabled.
func TestBlock21F3_ImportCauseMissingGoSum(t *testing.T) {
	b21fOffline(t)
	root := t.TempDir()
	b21fWrite(t, root, map[string]string{
		"go.mod": "module example.com/leakerr\n\ngo 1.21\n\nrequire example.com/nothere/missing v1.2.3\n",
		"x/x.go": "package x\n\nimport _ \"example.com/nothere/missing\"\n",
	})
	got := ImportCause(context.Background(), root, filepath.Join(root, "x", "x.go"), "example.com/nothere/missing")
	want := "missing go.sum entry; run: go mod tidy, or go get example.com/nothere/missing"
	if got != want {
		t.Fatalf("cause = %q, want %q", got, want)
	}
}

// SHAPE-008: a dependency that is in the module cache, with the go.sum deleted.
func TestBlock21F3_ImportCauseCachedModuleWithoutGoSum(t *testing.T) {
	b21fOffline(t)
	out, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Skip("go env GOMODCACHE failed")
	}
	dirs, _ := filepath.Glob(filepath.Join(strings.TrimSpace(string(out)), "golang.org", "x", "sys@v*"))
	if len(dirs) == 0 {
		t.Skip("golang.org/x/sys is not in the offline module cache")
	}
	version := strings.TrimPrefix(filepath.Base(dirs[0]), "sys@")
	root := t.TempDir()
	b21fWrite(t, root, map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.21\n\nrequire golang.org/x/sys " + version + "\n",
		"x/x.go": "package x\n\nimport \"golang.org/x/sys/unix\"\n\nvar _ = unix.Getpid\n",
	})
	got := ImportCause(context.Background(), root, filepath.Join(root, "x", "x.go"), "golang.org/x/sys/unix")
	if !strings.HasPrefix(got, "missing go.sum entry") {
		t.Fatalf("cause = %q, want the missing go.sum entry", got)
	}
}

// Root-awareness: the file lives in a nested module with its own replaced dependency, and the process runs from
// elsewhere. The root's module requires nothing, so a lookup run in the root finds no cause.
func TestBlock21F3_ImportCauseNestedModuleOwnsTheFile(t *testing.T) {
	b21fOffline(t)
	root := t.TempDir()
	b21fWrite(t, root, map[string]string{
		"go.mod":            "module example.com/root\n\ngo 1.21\n",
		"main.go":           "package main\n\nfunc main() {}\n",
		"nested/go.mod":     "module example.com/root/nested\n\ngo 1.21\n\nrequire example.com/dep v0.0.0\n\nreplace example.com/dep => ./dep\n",
		"nested/x/x.go":     "package x\n\nimport \"example.com/dep\"\n\nfunc X() int { return dep.Dep() }\n",
		"nested/dep/go.mod": "module example.com/dep\n\ngo 1.21\n\nrequire (\n\tgolang.org/x/nothere v1.0.0\n",
		"nested/dep/dep.go": "package dep\n\nfunc Dep() int { return 1 }\n",
	})
	t.Chdir(t.TempDir())
	got := ImportCause(context.Background(), root, filepath.Join(root, "nested", "x", "x.go"), "example.com/dep")
	if !strings.Contains(got, "nested/dep/go.mod") {
		t.Fatalf("cause = %q, want the parse error of nested/dep/go.mod", got)
	}
}

// A replaced go.mod that is fixed is no longer a cause: the cache follows the content of the replacement's go.mod.
func TestBlock21F3_ImportCauseFollowsFix(t *testing.T) {
	b21fOffline(t)
	root := t.TempDir()
	b21fWrite(t, root, map[string]string{
		"go.mod":     "module example.com/leakrep\n\ngo 1.21\n\nrequire example.com/dep v0.0.0\n\nreplace example.com/dep => ./dep\n",
		"x/x.go":     "package x\n\nimport \"example.com/dep\"\n\nfunc X() int { return dep.Dep() }\n",
		"dep/go.mod": "module example.com/dep\n\ngo 1.21\n\nrequire (\n",
		"dep/dep.go": "package dep\n\nfunc Dep() int { return 1 }\n",
	})
	file := filepath.Join(root, "x", "x.go")
	if got := ImportCause(context.Background(), root, file, "example.com/dep"); !strings.Contains(got, "dep/go.mod") {
		t.Fatalf("broken replacement: cause = %q", got)
	}
	b21fWrite(t, root, map[string]string{"dep/go.mod": "module example.com/dep\n\ngo 1.21\n"})
	if got := ImportCause(context.Background(), root, file, "example.com/dep"); got != "" {
		t.Fatalf("fixed replacement: cause = %q, want none", got)
	}
}

// An import that no module provides has no cause: the generic wording stays, nothing is invented.
func TestBlock21F3_ImportCauseUnknownStaysEmpty(t *testing.T) {
	b21fOffline(t)
	root := t.TempDir()
	b21fWrite(t, root, map[string]string{
		"go.mod": "module example.com/plain\n\ngo 1.21\n",
		"x/x.go": "package x\n\nimport _ \"example.com/none/such\"\n",
	})
	if got := ImportCause(context.Background(), root, filepath.Join(root, "x", "x.go"), "example.com/none/such"); got != "" {
		t.Fatalf("cause = %q, want none", got)
	}
}

// causeText keeps the first line only, drops the "module X:" prefix of a replacement, and shows paths relative to root.
func TestBlock21F3_CauseTextShape(t *testing.T) {
	root := "/work/ws"
	msg := "module ./dep: parsing dep/go.mod: /work/ws/dep/go.mod:7: syntax error\n\tmore detail"
	if got, want := causeText(msg, "example.com/dep", root), "parsing dep/go.mod: dep/go.mod:7: syntax error"; got != want {
		t.Errorf("causeText = %q, want %q", got, want)
	}
	if got := causeText("no required module provides package example.com/x; to add it:\n\tgo get example.com/x", "example.com/x", root); got != "" {
		t.Errorf("generic error gave cause %q", got)
	}
	if got := causeText("package x is not in std (/usr/lib/go/src/x)", "x", root); got != "" {
		t.Errorf("not-in-std gave cause %q", got)
	}
	if got := relativePaths("open /work/ws/dep/go.mod: no", root); got != "open dep/go.mod: no" {
		t.Errorf("path inside root = %q", got)
	}
	if got := relativePaths("open /work/mod/go.mod: no", root); got != "open ../mod/go.mod: no" {
		t.Errorf("path beside root = %q", got)
	}
	if got := relativePaths("open /a/go.mod", "/work/ws/deep/er"); got != "open /a/go.mod" {
		t.Errorf("path far outside root = %q", got)
	}
	if got := relativePaths("see /work/ws/a/b.go and /work/wsx/c", root); got != "see a/b.go and ../wsx/c" {
		t.Errorf("path boundary = %q", got)
	}
	long := strings.Repeat("é", 300)
	if got := truncateCause(long, 11); !strings.HasSuffix(got, "...") || len(got) > 14 {
		t.Errorf("truncateCause = %q", got)
	}
	if got := causeText(`malformed import path "-toolexec=x": leading dash`, "-toolexec=x", root); got != "" {
		t.Errorf("malformed import path gave cause %q, want none", got)
	}
}

// Security: an import path that starts with a dash is a package name for the go command, never a flag. A -toolexec or
// -modfile given as the path does not run or change the lookup: the answer is empty, and the cause the go command
// reports for the owning module ("." of the broken replacement) is not returned for the path.
func TestBlock21F3_ImportCauseDashPathIsNotAFlag(t *testing.T) {
	b21fOffline(t)
	root := t.TempDir()
	b21fWrite(t, root, map[string]string{
		"go.mod":     "module example.com/leakrep\n\ngo 1.21\n\nrequire example.com/dep v0.0.0\n\nreplace example.com/dep => ./dep\n",
		"x/x.go":     "package x\n\nimport \"example.com/dep\"\n\nfunc X() int { return dep.Dep() }\n",
		"dep/go.mod": "module example.com/dep\n\ngo 1.21\n\nrequire (\n\tgolang.org/x/nothere v1.0.0\n",
		"dep/dep.go": "package dep\n\nfunc Dep() int { return 1 }\n",
	})
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "tool.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "x", "x.go")
	for _, p := range []string{"-toolexec=" + script, "-modfile=" + script} {
		start := time.Now()
		if got := ImportCause(context.Background(), root, file, p); got != "" {
			t.Errorf("ImportCause(%q) = %q, want none", p, got)
		}
		if elapsed := time.Since(start); elapsed > 20*time.Second {
			t.Errorf("ImportCause(%q) took %s", p, elapsed)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the -toolexec program ran")
	}
}

// The go list output read for one lookup is capped: bytes past the cap are discarded, the writer still reports them as
// written (so the command is not broken), and over tells the lookup that the answer is not readable.
func TestBlock21F3_CappedBufferDiscardsPastCap(t *testing.T) {
	c := &cappedBuffer{max: 10}
	if n, err := c.Write([]byte("0123456789abcdef")); n != 16 || err != nil || c.buf.String() != "0123456789" || !c.over {
		t.Fatalf("first write: n=%d err=%v buf=%q over=%v", n, err, c.buf.String(), c.over)
	}
	if n, err := c.Write([]byte("zz")); n != 2 || err != nil || c.buf.Len() != 10 {
		t.Fatalf("write past the cap: n=%d err=%v len=%d", n, err, c.buf.Len())
	}
	small := &cappedBuffer{max: maxCauseOutput}
	big := strings.Repeat("x", 3<<20)
	for i := 0; i < len(big); i += 64 << 10 {
		if _, err := small.Write([]byte(big[i : i+64<<10])); err != nil {
			t.Fatal(err)
		}
	}
	if small.buf.Len() != maxCauseOutput || !small.over {
		t.Errorf("3 MB written: kept %d bytes (want %d), over=%v", small.buf.Len(), maxCauseOutput, small.over)
	}
}
