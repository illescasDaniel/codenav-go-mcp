package exclude

import (
	"os/exec"
	"strings"
	"testing"
)

// Regression (WRX-004 / E follow-up): RelativeTo and OutsideRelative share one `../` walk, and each keeps its own
// policy on the same inputs: RelativeTo leaves "/" out and keeps toolchain paths absolute, OutsideRelative climbs
// to "/" and never prints a toolchain path absolute.
func TestBlock21F2_RelativePolicies(t *testing.T) {
	root := "/work/ws/app" // ancestors: /work/ws, /work, / (three steps to "/")
	const up3 = "../../../"

	cases := []struct {
		name, path, relative, outside string
	}{
		{"inside root", "/work/ws/app/main/x.go", "main/x.go", "main/x.go"},
		{"sibling dir", "/work/ws/other/y.go", "../other/y.go", "../other/y.go"},
		{"sibling two levels up", "/work/zz/w.go", "../../zz/w.go", "../../zz/w.go"},
		{"only / in common", "/usr/share/z.go", "/usr/share/z.go", up3 + "usr/share/z.go"},
	}
	for _, c := range cases {
		if got := RelativeTo(c.path, root); got != c.relative {
			t.Errorf("%s: RelativeTo(%q) = %q, want %q", c.name, c.path, got, c.relative)
		}
		if got := OutsideRelative(c.path, root); got != c.outside {
			t.Errorf("%s: OutsideRelative(%q) = %q, want %q", c.name, c.path, got, c.outside)
		}
	}

	// The module cache is a toolchain path: RelativeTo keeps it absolute (the same as before), OutsideRelative walks
	// to it with `../` so the home directory it lives in does not show.
	out, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Skip("go env GOMODCACHE unavailable")
	}
	modcache := strings.TrimSpace(string(out))
	dep := modcache + "/example.com/dep@v1.0.0/d.go"
	if got := RelativeTo(dep, root); got != dep {
		t.Errorf("RelativeTo(module cache) = %q, want the absolute path %q", got, dep)
	}
	if got, want := OutsideRelative(dep, root), up3+strings.TrimPrefix(dep, "/"); got != want {
		t.Errorf("OutsideRelative(module cache) = %q, want %q", got, want)
	}
	if strings.HasPrefix(OutsideRelative(dep, root), "/") {
		t.Errorf("OutsideRelative printed the module cache absolute: %q", OutsideRelative(dep, root))
	}
}

// The two policies agree wherever the path shares a directory with the root that is not "/": both print the
// same `../` spelling there.
func TestBlock21F2_PoliciesAgreeBelowSharedAncestor(t *testing.T) {
	root := "/a/b/c/d/e/f"
	for path, want := range map[string]string{
		"/a/b/x.go":       "../../../../x.go",
		"/a/b/c/y/z.go":   "../../../y/z.go",
		"/a/b/c/d/e/f/w":  "w",
		"/a/b/c/d/e/q.go": "../q.go",
	} {
		if got := RelativeTo(path, root); got != want {
			t.Errorf("RelativeTo(%q) = %q, want %q", path, got, want)
		}
		if got := OutsideRelative(path, root); got != want {
			t.Errorf("OutsideRelative(%q) = %q, want %q", path, got, want)
		}
	}
}
