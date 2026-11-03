package buildscope

import (
	"context"
	"fmt"
	"go/build"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/illescasDaniel/codenav-go-mcp/internal/gomod"
)

// linuxAMD64 is the build of the tests: linux/amd64 with the given tags only.
func linuxAMD64(tags ...string) *build.Context {
	c := build.Default
	c.GOOS, c.GOARCH, c.BuildTags = "linux", "amd64", tags
	return &c
}

func write(t *testing.T, root, rel, src string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

var kindNames = map[Kind]string{Use: "use", Decl: "decl", Member: "member", Asm: "asm"}

// summary renders hits as sorted "file:line kind" lines, relative to root.
func summary(root string, hs []Hit) []string {
	var out []string
	for _, h := range hs {
		rel, _ := filepath.Rel(root, h.Path)
		out = append(out, fmt.Sprintf("%s:%d %s", filepath.ToSlash(rel), h.Line, kindNames[h.Kind]))
	}
	sort.Strings(out)
	return out
}

func equalLines(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// bed is a module example.com/m whose package p (the declaring package) holds Plat and Run.
func bed(t *testing.T) (root, declFile string) {
	t.Helper()
	root = t.TempDir()
	write(t, root, "go.mod", "module example.com/m\n\ngo 1.24\n")
	declFile = write(t, root, "p/p.go", "package p\n\nfunc Plat(n int) int { return n }\n\nfunc Run() int { return Plat(1) }\n")
	return root, declFile
}

func TestExcludedReasons(t *testing.T) {
	c := linuxAMD64()
	dir := t.TempDir()
	for i, tc := range []struct {
		name, src, reason string
		excluded          bool
	}{
		{"a.go", "package p\n", "", false},
		{"a_linux.go", "package p\n", "", false},
		{"a_windows.go", "package p\n", "file name suffix _windows", true},
		{"a_linux_amd64.go", "package p\n", "", false},
		{"a_linux_arm64.go", "package p\n", "file name suffix _linux_arm64", true},
		{"linux_amd64.go", "package p\n", "", false},
		{"a_windows_test.go", "package p\n", "file name suffix _windows", true},
		{"a_test.go", "package p\n", "", false},
		{"a.go", "//go:build linux && arm64\n\npackage p\n", "//go:build linux && arm64", true},
		{"a.go", "//go:build ignore\n\npackage main\n", "//go:build ignore", true},
		{"_a.go", "package p\n", "its name starts with _ or . (the go tool ignores it)", true},
	} {
		p := write(t, dir, fmt.Sprintf("case%d/%s", i, tc.name), tc.src)
		reason, excluded := Excluded(c, p)
		if excluded != tc.excluded || reason != tc.reason {
			t.Errorf("%s %q: Excluded = (%q, %v), want (%q, %v)", tc.name, tc.src, reason, excluded, tc.reason, tc.excluded)
		}
	}
}

func TestNameConstraintAndNeverBuilt(t *testing.T) {
	for file, want := range map[string]string{
		"x_windows.go": "file name suffix _windows", "x_linux_arm64.go": "file name suffix _linux_arm64",
		"x_amd64.go": "file name suffix _amd64", "x_windows_test.go": "file name suffix _windows",
		"linux.go": "", "x_y.go": "", "x_test.go": "",
	} {
		if got := NameConstraint(file); got != want {
			t.Errorf("NameConstraint(%q) = %q, want %q", file, got, want)
		}
	}
	for cons, want := range map[string]bool{
		"//go:build ignore": true, "//go:build ignore || !ignore": false, "//go:build linux": false,
		"//go:build !linux": false, "// not a constraint": false,
	} {
		if got := NeverBuilt(cons); got != want {
			t.Errorf("NeverBuilt(%q) = %v, want %v", cons, got, want)
		}
	}
}

// BUG-MOVE-004: the constraint a file puts on its declarations, normalized, so that two files with the same
// constraint compare equal whatever the spelling of their lines.
func TestFileConstraint(t *testing.T) {
	for _, c := range []struct{ path, src, want string }{
		{"tags.go", "//go:build linux\n\npackage src\n", "//go:build linux"},
		{"tags.go", "// +build linux,amd64\n\npackage src\n", "//go:build linux && amd64"},
		// Every +build line must hold: the go tool ANDs them, so both constraints carry over.
		{"tags.go", "// +build linux\n// +build amd64\n\npackage src\n", "//go:build linux && amd64"},
		{"tags.go", "// +build linux amd64\n// +build !cgo\n\npackage src\n", "//go:build (linux || amd64) && !cgo"},
		{"tags.go", "//go:build (linux) && (amd64)\n\npackage src\n", "//go:build linux && amd64"},
		{"tags.go", "//go:build amd64 && linux\n\npackage src\n", "//go:build amd64 && linux"},
		{"tags.go", "package src\n", ""},
		{"x_windows.go", "package src\n", "//go:build windows"},
		{"x_linux_amd64.go", "package src\n", "//go:build linux && amd64"},
		{"x_windows.go", "//go:build linux\n\npackage src\n", "//go:build linux && windows"},
		{"x_test.go", "package src\n", ""},
	} {
		if got := FileConstraint(c.path, c.src); got != c.want {
			t.Errorf("FileConstraint(%q, %q) = %q, want %q", c.path, c.src, got, c.want)
		}
	}
}

// Equivalent compares the builds two constraints admit, not their spelling.
func TestEquivalent(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"//go:build linux && linux", "//go:build linux", true},
		{"//go:build amd64 && linux", "//go:build linux && amd64", true},
		{"", "//go:build linux || !linux", true},
		{"", "", true},
		{"", "//go:build linux", false},
		{"//go:build linux", "//go:build windows", false},
		{"//go:build linux && windows", "//go:build linux", false},
	} {
		if got := Equivalent(c.a, c.b); got != c.want {
			t.Errorf("Equivalent(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
	if got := FileConstraint("y_linux.go", "package y\n"); !Equivalent(got, "//go:build linux") {
		t.Errorf("a _linux file has the constraint linux, got %q", got)
	}
	// Two // +build lines are linux AND amd64, not linux alone.
	two := FileConstraint("y.go", "// +build linux\n// +build amd64\n\npackage y\n")
	if !Equivalent(two, "//go:build linux && amd64") {
		t.Errorf("two +build lines should equal linux && amd64, got %q", two)
	}
	if Equivalent(two, "//go:build linux") {
		t.Errorf("two +build lines must not be equivalent to linux alone, got %q", two)
	}
}

func TestContextReadsBuildFlagTags(t *testing.T) {
	t.Setenv(gomod.EnvBuildFlags, "-race -tags=integration,e2e --tags=mock")
	if got := Context().BuildTags; strings.Join(got, ",") != "integration,e2e,mock" {
		t.Errorf("tags %v", got)
	}
	t.Setenv(gomod.EnvBuildFlags, "-tags wasm")
	if got := Context().BuildTags; strings.Join(got, ",") != "wasm" {
		t.Errorf("tags %v", got)
	}
	t.Setenv(gomod.EnvBuildFlags, "")
	if got := Context().BuildTags; len(got) != 0 {
		t.Errorf("no flags, tags %v", got)
	}
}

func TestMentionsSameDirectory(t *testing.T) {
	root, declFile := bed(t)
	write(t, root, "p/x_windows.go", "package p\n\nfunc WinRun() int { return Plat(2) }\n")
	write(t, root, "p/y_linux.go", "package p\n\nfunc Y() int { return Plat(3) }\n")                     // built: not reported
	write(t, root, "p/z.go", "//go:build integration\n\npackage p\n\nfunc Z() int { return Plat(4) }\n") // excluded without the tag
	write(t, root, "p/gen.go", "//go:build ignore\n\npackage main\n\nfunc main() { _ = Plat(0) }\n")     // never built
	write(t, root, "p/_scratch.go", "package p\n\nfunc S() int { return Plat(5) }\n")                    // ignored by the go tool
	write(t, root, "p/decl_windows.go", "package p\n\nfunc Plat(n int) int { return n }\n")
	write(t, root, "p/quiet_windows.go", "package p\n\n// Plat is named in a comment.\nvar label = \"Plat\"\n\ntype box struct{ Plat int }\n\nfunc Sh() int {\n\tPlat := 3\n\tb := box{}\n\treturn b.Plat + Plat\n}\n")
	q := Query{Root: root, DeclFile: declFile, Name: "Plat"}

	equalLines(t, summary(root, Mentions(linuxAMD64(), q)), []string{
		"p/decl_windows.go:3 decl", "p/x_windows.go:3 use", "p/z.go:5 use",
	})
	// With the integration tag, z.go is part of the build (gopls sees it), so it is not reported.
	equalLines(t, summary(root, Mentions(linuxAMD64("integration"), q)), []string{
		"p/decl_windows.go:3 decl", "p/x_windows.go:3 use",
	})
}

func TestMentionsReasonAndAsm(t *testing.T) {
	root, declFile := bed(t)
	write(t, root, "p/x_windows.go", "package p\n\nfunc WinRun() int { return Plat(2) }\n")
	write(t, root, "p/z.go", "//go:build integration\n\npackage p\n\nfunc Z() int { return Plat(4) }\n")
	write(t, root, "p/asm_amd64.s", "#include \"textflag.h\"\n\nTEXT ·Plat(SB), NOSPLIT, $0-16\n\tRET\n")
	write(t, root, "p/asm_arm64.s", "TEXT ·Plat(SB), NOSPLIT, $0-16\n\tRET\n")
	hs := Mentions(linuxAMD64(), Query{Root: root, DeclFile: declFile, Name: "Plat"})
	reasons := map[string]string{}
	for _, h := range hs {
		rel, _ := filepath.Rel(root, h.Path)
		reasons[filepath.ToSlash(rel)+" "+kindNames[h.Kind]] = h.Reason
	}
	if reasons["p/x_windows.go use"] != "file name suffix _windows" {
		t.Errorf("use reason: %v", reasons)
	}
	if r, ok := reasons["p/asm_amd64.s asm"]; !ok || r != "" {
		t.Errorf("built asm file should be a hit without a reason: %v", reasons)
	}
	if r := reasons["p/asm_arm64.s asm"]; r != "file name suffix _arm64" {
		t.Errorf("excluded asm file reason %q in %v", r, reasons)
	}
	if _, ok := reasons["p/z.go use"]; !ok {
		t.Errorf("the integration-tagged file is excluded without the tag and must be reported: %v", reasons)
	}
	// Positions are 1-based; the column is the name's own (the call is "return Plat(2)" on line 3).
	for _, h := range hs {
		if h.Kind == Use && filepath.Base(h.Path) == "x_windows.go" && (h.Line != 3 || h.Col != 28) {
			t.Errorf("use at %d:%d, want 3:28", h.Line, h.Col)
		}
	}
}

func TestMentionsOtherPackagesNeedAnImport(t *testing.T) {
	root, declFile := bed(t)
	write(t, root, "q/q_windows.go", "package q\n\nimport alias \"example.com/m/p\"\n\nfunc Q() int { return alias.Plat(6) }\n")
	write(t, root, "q/r_windows.go", "package q\n\nimport \"example.com/m/p\"\n\nfunc R() int { return p.Plat(7) }\n")
	write(t, root, "q/own_windows.go", "package q\n\nfunc Plat(n int) int { return n }\n\nfunc O() int { return Plat(8) }\n")
	write(t, root, "q/shadow_windows.go", "package q\n\nimport \"example.com/m/p\"\n\nfunc S(p int) int { return p }\n")
	q := Query{Root: root, DeclFile: declFile, Name: "Plat", ImportPath: "example.com/m/p"}

	// The package q's own Plat is a different object; the files that import p are the uses.
	equalLines(t, summary(root, Mentions(linuxAMD64(), q)), []string{"q/q_windows.go:5 use", "q/r_windows.go:5 use"})

	// An unexported name is not reachable from other packages.
	q.Name = "plat"
	if got := Mentions(linuxAMD64(), q); len(got) != 0 {
		t.Errorf("unexported name in other packages: %v", summary(root, got))
	}
}

func TestMentionsMembers(t *testing.T) {
	root, declFile := bed(t)
	write(t, root, "p/m_windows.go", "package p\n\ntype Store struct{}\n\nfunc (Store) Get() int { return 1 }\n\nfunc useGet(s Store) int { return s.Get() }\n")
	q := Query{Root: root, DeclFile: declFile, Name: "Get", Member: true}
	equalLines(t, summary(root, Mentions(linuxAMD64(), q)), []string{"p/m_windows.go:5 member", "p/m_windows.go:7 member"})
	// As a package-level name, Get is not declared at top level (the method is a member): nothing to report.
	q.Member = false
	if got := Mentions(linuxAMD64(), q); len(got) != 0 {
		t.Errorf("package-level Get: %v", summary(root, got))
	}
}

func TestMentionsReadsTextsOverDisk(t *testing.T) {
	root, declFile := bed(t)
	x := write(t, root, "p/x_windows.go", "package p\n\nfunc WinRun() int { return Plat(2) }\n")
	q := Query{Root: root, DeclFile: declFile, Name: "Plat", Texts: map[string]string{
		x: "package p\n\nfunc WinRun() int { return 2 }\n",
	}}
	if got := Mentions(linuxAMD64(), q); len(got) != 0 {
		t.Errorf("the edited text has no mention: %v", summary(root, got))
	}
}

// An edit's text decides whether its file is excluded: a file not on disk yet (created by the edit) is judged by
// its header, and a header the edit changes flips the answer.
func TestExcludedTextJudgesTheEditedText(t *testing.T) {
	c := linuxAMD64()
	root := t.TempDir()
	p := filepath.Join(root, "p", "new.go")
	if reason, ex := ExcludedText(c, p, "//go:build integration\n\npackage p\n"); !ex || reason != "//go:build integration" {
		t.Errorf("integration-tagged text: got %q, %v", reason, ex)
	}
	if reason, ex := ExcludedText(c, p, "package p\n"); ex {
		t.Errorf("plain text is included, got excluded (%q)", reason)
	}
	if reason, ex := ExcludedText(c, filepath.Join(root, "p", "x_windows.go"), "package p\n"); !ex || reason != "file name suffix _windows" {
		t.Errorf("_windows name: got %q, %v", reason, ex)
	}
}

func TestExclusive(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"//go:build linux", "//go:build windows", true},
		{"//go:build linux && windows", "//go:build linux", true}, // no build has both
		{"//go:build linux", "//go:build integration", false},     // a build with both exists
		{"//go:build linux", "//go:build linux", false},
		// An android build also builds the _linux files; an android file and a windows file never meet.
		{"//go:build android", "//go:build linux", false},
		{"//go:build android", "//go:build windows", true},
		{"//go:build ios", "//go:build darwin", false},
		{"//go:build illumos", "//go:build solaris", false},
		{"", "//go:build linux", false},
		{"", "", false},
		{"//go:build linux &&", "//go:build windows", false}, // unparsable: not exclusive
	} {
		if got := Exclusive(c.a, c.b); got != c.want {
			t.Errorf("Exclusive(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
	// A _linux and a _windows file never build together; a _linux file and an integration file can.
	lin := FileConstraint("f_linux.go", "package f\n")
	if !Exclusive(lin, FileConstraint("f_windows.go", "package f\n")) {
		t.Errorf("f_linux.go and f_windows.go must be exclusive")
	}
	if Exclusive(lin, FileConstraint("f_integration.go", "//go:build integration\n\npackage f\n")) {
		t.Errorf("f_linux.go and an integration file can be built together")
	}
}

// MentionsAll reads the tree once for any number of names: the walk count is the check.
func TestMentionsAllWalksTheTreeOnce(t *testing.T) {
	root, declFile := bed(t)
	rFile := write(t, root, "r/r.go", "package r\n\nvar Thing = 1\n")
	write(t, root, "q/q_windows.go", "package q\n\nimport \"example.com/m/p\"\n\nvar _ = []any{p.Plat, p.Run, p.Name07}\n")
	write(t, root, "s/s_windows.go", "package s\n\nimport \"example.com/m/r\"\n\nvar _ = r.Thing\n")
	names := []string{"", "_", "Plat", "Run"}
	for i := 0; i < 60; i++ {
		names = append(names, fmt.Sprintf("Name%02d", i))
	}
	res, err := MentionsAll(context.Background(), linuxAMD64(), root, nil, []Target{
		{DeclFile: declFile, ImportPath: "example.com/m/p", Names: names},
		{DeclFile: rFile, ImportPath: "example.com/m/r", Names: []string{"Thing", "Thing"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Walks != 1 {
		t.Errorf("walks = %d, want 1 for any number of names and targets", res.Walks)
	}
	var got []string
	for _, h := range res.Hits {
		got = append(got, h.Name+" "+filepath.ToSlash(strings.TrimPrefix(h.Path, root+string(filepath.Separator))))
	}
	sort.Strings(got)
	equalLines(t, got, []string{"Name07 q/q_windows.go", "Plat q/q_windows.go", "Run q/q_windows.go", "Thing s/s_windows.go"})
}

func TestMentionsAllWalksOnlyForOtherPackages(t *testing.T) {
	root, declFile := bed(t)
	res, err := MentionsAll(context.Background(), linuxAMD64(), root, nil, []Target{{DeclFile: declFile, Names: []string{"Plat"}}})
	if err != nil || res.Walks != 0 {
		t.Errorf("a target without an import path walks nothing: walks = %d, err = %v", res.Walks, err)
	}
}

func TestMentionsAllStopsWhenCancelled(t *testing.T) {
	root, declFile := bed(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := MentionsAll(ctx, linuxAMD64(), root, nil, []Target{{DeclFile: declFile, ImportPath: "example.com/m/p", Names: []string{"Plat"}}})
	if err != context.Canceled {
		t.Errorf("a cancelled scan returns the context error, got %v", err)
	}
}
