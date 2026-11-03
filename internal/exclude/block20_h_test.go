package exclude

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestFile(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Regression (SHAPE-001): an ordinary Go package named build, dist or venv is editable by the write tools.
func TestWriteToolsEditPackagesNamedBuildDistVenv(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"internal/build/build.go", "dist/dist.go", "venv/venv.go"} {
		writeTestFile(t, filepath.Join(root, f))
		if IsExcluded(filepath.Join(root, f), root) {
			t.Errorf("IsExcluded(%s) = true, want false: the directory holds Go files", f)
		}
	}
	// A new file in such a directory is a write into a Go package too.
	if IsExcluded(filepath.Join(root, "internal", "build", "new.go"), root) {
		t.Errorf("a new .go file in internal/build must not be excluded")
	}
	// The symbol index keeps skipping the names.
	for _, n := range []string{"build", "dist", "venv"} {
		if !IsDirName(n) {
			t.Errorf("IsDirName(%q) = false, want true", n)
		}
	}
}

// build, dist and venv stay excluded where they hold no Go files, and the other names are always excluded.
func TestWriteToolsStillExcludeNonGoBuildDirs(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "build", "notes.txt"))
	writeTestFile(t, filepath.Join(root, "dist", "sub", "x.go"))
	writeTestFile(t, filepath.Join(root, "node_modules", "pkg", "a.go"))
	cases := map[string]bool{
		filepath.Join(root, "build", "notes.txt"):          true,
		"build/notes.txt":                                  true, // relative to root
		filepath.Join(root, "dist", "sub", "x.go"):         true, // dist holds no Go file directly
		filepath.Join(root, "node_modules", "pkg", "a.go"): true,
		filepath.Join(root, "main.go"):                     false,
	}
	for p, want := range cases {
		if got := IsExcluded(p, root); got != want {
			t.Errorf("IsExcluded(%s) = %v, want %v", p, got, want)
		}
	}
}

// Regression (SHAPE-002): a project directory named go-build is not cgo output.
func TestGoBuildProjectDirIsNotCgoGenerated(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "go-build", "use.go")
	if IsCgoGenerated(p) {
		t.Errorf("IsCgoGenerated(%s) = true, want false: go-build is a project directory here", p)
	}
	// cgo's own temporary directory is still recognised.
	cgo := filepath.Join(os.TempDir(), "go-build12345", "b001", "_cgo_gotypes.go")
	if !IsCgoGenerated(cgo) {
		t.Errorf("IsCgoGenerated(%s) = false, want true", cgo)
	}
}

// GOTMPDIR is where go build puts cgo's work directory; a go-build<random>/b<nnn>/ file there is cgo output,
// and so is the same file reached through a symlinked GOTMPDIR (the path gopls may report on macOS).
func TestCgoWorkDirUnderGOTMPDIRAndSymlink(t *testing.T) {
	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "tmplink")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	t.Setenv("GOTMPDIR", link)
	for _, p := range []string{
		filepath.Join(link, "go-build777", "b001", "_cgo_gotypes.go"),
		filepath.Join(realDir, "go-build777", "b001", "_cgo_gotypes.go"),
	} {
		if !IsCgoGenerated(p) {
			t.Errorf("IsCgoGenerated(%s) = false, want true", p)
		}
	}
	// A project directory go-build under the same temp root is still not cgo output.
	if bare := filepath.Join(realDir, "proj", "go-build", "use.go"); IsCgoGenerated(bare) {
		t.Errorf("IsCgoGenerated(%s) = true, want false", bare)
	}
}

// The build-cache fan-out shape is recognised even when its root is unknown, and a bare project file is not.
func TestCgoCacheShapeFallback(t *testing.T) {
	if !IsCgoGenerated("/nowhere/cache/go-build/ab/0123-d") {
		t.Error("build-cache fan-out path should count as cgo output")
	}
	if IsCgoGenerated("/nowhere/proj/go-build/use.go") {
		t.Error("bare project go-build/use.go must not count as cgo output")
	}
}
