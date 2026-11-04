package gomod

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Block 22 item 3 (b): the go get hint names the required module the import path belongs to, not the import path.
// A missing go.sum entry for example.com/nothere/sub is fixed by `go get example.com/nothere` (required in go.mod).
func TestBlock22_GoSumHintNamesRequiredModule(t *testing.T) {
	b21fOffline(t)
	root := t.TempDir()
	b21fWrite(t, root, map[string]string{
		"go.mod": "module example.com/hintmod\n\ngo 1.21\n\nrequire example.com/nothere v1.2.3\n",
		"x/x.go": "package x\n\nimport _ \"example.com/nothere/sub\"\n",
	})
	got := ImportCause(context.Background(), root, filepath.Join(root, "x", "x.go"), "example.com/nothere/sub")
	want := "missing go.sum entry; run: go mod tidy, or go get example.com/nothere"
	if got != want {
		t.Fatalf("cause = %q, want %q", got, want)
	}
}

// Block 22 item 3 (b): the longest required module path that is a path prefix of the import path wins, and a module path
// that is only a string prefix (example.com/a for example.com/ab) is not a match.
func TestBlock22_GoSumHintLongestRequiredPrefix(t *testing.T) {
	for _, c := range []struct {
		importPath string
		required   []string
		want       string
	}{
		{"example.com/a/b/c", []string{"example.com/a", "example.com/a/b"}, "missing go.sum entry; run: go mod tidy, or go get example.com/a/b"},
		{"example.com/a/b", []string{"example.com/a/b"}, "missing go.sum entry; run: go mod tidy, or go get example.com/a/b"},
		{"example.com/ab/c", []string{"example.com/a"}, "missing go.sum entry; run: go mod tidy"},
		{"golang.org/x/net/html", []string{"golang.org/x/text"}, "missing go.sum entry; run: go mod tidy"},
		{"example.com/x", nil, "missing go.sum entry; run: go mod tidy"},
	} {
		if got := goSumCause(c.importPath, c.required); got != c.want {
			t.Errorf("goSumCause(%q, %v) = %q, want %q", c.importPath, c.required, got, c.want)
		}
	}
}

// Block 22 item 3 (b): a missing go.sum entry for an import path that no required module of the owning go.mod covers
// names only `go mod tidy`; the import path itself is never offered as a go get target.
func TestBlock22_GoSumHintNeverNamesUnrequiredPath(t *testing.T) {
	b21fOffline(t)
	root := t.TempDir()
	b21fWrite(t, root, map[string]string{
		"go.mod":     "module example.com/hintmod2\n\ngo 1.21\n\nrequire example.com/mid v0.0.0\n\nreplace example.com/mid => ./mid\n",
		"x/x.go":     "package x\n\nimport _ \"example.com/mid\"\n",
		"mid/go.mod": "module example.com/mid\n\ngo 1.21\n\nrequire example.com/leaf/deep v1.0.0\n",
		"mid/mid.go": "package mid\n",
	})
	got := ImportCause(context.Background(), root, filepath.Join(root, "x", "x.go"), "example.com/leaf/deep/pkg")
	if want := "missing go.sum entry; run: go mod tidy"; got != want {
		t.Errorf("cause = %q, want %q (the import path is not a required module)", got, want)
	}
}

// Block 22 item 3 (a): the cache key covers the go.mod of a directory that a go.work replaces a module with. A replacement
// that breaks after the first lookup is seen at once, not answered from the cache with the old (empty) cause.
func TestBlock22_ImportCauseFollowsGoWorkReplaceFix(t *testing.T) {
	b21fOffline(t)
	t.Setenv("GOWORK", "")
	root := t.TempDir()
	b21fWrite(t, root, map[string]string{
		"go.mod":     "module example.com/wsroot\n\ngo 1.21\n\nrequire example.com/dep v0.0.0\n",
		"go.work":    "go 1.21\n\nuse .\n\nreplace example.com/dep => ./dep\n",
		"x/x.go":     "package x\n\nimport \"example.com/dep\"\n\nfunc X() int { return dep.Dep() }\n",
		"dep/go.mod": "module example.com/dep\n\ngo 1.21\n",
		"dep/dep.go": "package dep\n\nfunc Dep() int { return 1 }\n",
	})
	file := filepath.Join(root, "x", "x.go")
	if got := ImportCause(context.Background(), root, file, "example.com/dep"); got != "" {
		t.Fatalf("working replacement: cause = %q, want none", got)
	}
	b21fWrite(t, root, map[string]string{"dep/go.mod": "module example.com/dep\n\ngo 1.21\n\nrequire (\n"})
	if got := ImportCause(context.Background(), root, file, "example.com/dep"); !strings.Contains(got, "dep/go.mod") {
		t.Fatalf("broken go.work replacement: cause = %q, want the parse error of dep/go.mod (cache not invalidated)", got)
	}
}

// replaceFiles lists the go.mod and go.sum of the local replacements of the governing go.work, relative to the go.work.
func TestBlock22_ReplaceFilesIncludeGoWork(t *testing.T) {
	t.Setenv("GOWORK", "")
	root := t.TempDir()
	b21fWrite(t, root, map[string]string{
		"go.mod":  "module example.com/wsroot\n\ngo 1.21\n",
		"go.work": "go 1.21\n\nuse .\n\nreplace (\n\texample.com/dep => ./dep\n\texample.com/other => example.com/other v1.0.0\n)\n",
	})
	files := replaceFiles(root)
	want := filepath.Join(root, "dep", "go.mod")
	found := false
	for _, f := range files {
		if f == want {
			found = true
		}
		if strings.Contains(f, "other") {
			t.Errorf("a replacement by version has files: %q", f)
		}
	}
	if !found {
		t.Errorf("replaceFiles = %v, want %s", files, want)
	}
}
