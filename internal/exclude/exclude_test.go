package exclude

import "testing"

// Round 13 (item 4): a path sharing an ancestor with the root is shown with "../" for up to three levels.
func TestRelativeToAncestors(t *testing.T) {
	root := "/work/proj/_deps/dep"
	for path, want := range map[string]string{
		"/work/proj/_deps/dep/dep.go": "dep.go",
		"/work/proj/_deps/other/o.go": "../other/o.go",
		"/work/proj/main.go":          "../../main.go",
		"/work/proj/nested/usedep.go": "../../nested/usedep.go",
		"/work/x.go":                  "../../../x.go",
		"/elsewhere/y.go":             "/elsewhere/y.go", // only "/" in common
	} {
		if got := RelativeTo(path, root); got != want {
			t.Errorf("RelativeTo(%q) = %q, want %q", path, got, want)
		}
	}
	// More than three levels up stays absolute.
	if got, want := RelativeTo("/a/b/x.go", "/a/b/c/d/e/f"), "/a/b/x.go"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	// One level, as before.
	if got, want := RelativeTo("/a/b/sib/x.go", "/a/b/c"), "../sib/x.go"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
