package tools

import "testing"

// WRX-004: a path outside the workspace is shown with `../` steps at any depth, toolchain paths included.
func TestBlock20EOutsideRelativeWalksUpAtAnyDepth(t *testing.T) {
	cases := []struct{ root, path, want string }{
		{"/home/u/proj/ws", "/home/u/go/pkg/mod/x@v1/a.go", "../../go/pkg/mod/x@v1/a.go"},
		{"/tmp/t/ws", "/usr/lib/go/src/strings/strings.go", "../../../usr/lib/go/src/strings/strings.go"},
		{"/a/ws", "/a/ws/x.go", "x.go"},
		{"/a/ws", "/a/dep/x.go", "../dep/x.go"},
	}
	for _, c := range cases {
		if got := outsideRelative(c.path, c.root); got != c.want {
			t.Errorf("outsideRelative(%q, %q) = %q, want %q", c.path, c.root, got, c.want)
		}
	}
}
