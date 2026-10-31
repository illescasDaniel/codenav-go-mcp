package tools

import (
	"strings"
	"testing"
)

func TestDropNulls(t *testing.T) {
	got := string(dropNulls([]byte(`{"name":null,"line":3,"x": null}`)))
	if strings.Contains(got, "null") || !strings.Contains(got, `"line":3`) {
		t.Errorf("got %s", got)
	}
	in := []byte(`{"a":1}`)
	if string(dropNulls(in)) != string(in) {
		t.Error("input without nulls must be unchanged")
	}
}

func TestTidyHoverDropsBogusLinks(t *testing.T) {
	for _, link := range []string{
		"[`outdep.Hello` on pkg.go.dev](https://pkg.go.dev/../outdep@#Hello)",
		"[`x.F` on pkg.go.dev](https://pkg.go.dev/./x@#F)",
		"[`x.F` on pkg.go.dev](https://pkg.go.dev/example.com/x@#F)",
		"[`main.F` on pkg.go.dev](https://pkg.go.dev/command-line-arguments#F)",
		"[`x.F` on pkg.go.dev](https://pkg.go.dev/example.com/x#F)",
		"[`x` on pkg.go.dev](https://pkg.go.dev/example.com/x/sub)",
	} {
		if got := tidyHover("func F()\n\n---\n\n" + link); strings.Contains(got, "pkg.go.dev") {
			t.Errorf("%s kept: %q", link, got)
		}
	}
	for _, link := range []string{
		"[`fmt.Println` on pkg.go.dev](https://pkg.go.dev/fmt#Println)",
		"[`x.F` on pkg.go.dev](https://pkg.go.dev/example.com/x@v1.2.3#F)",
	} {
		if got := tidyHover("func F()\n\n---\n\n" + link); !strings.Contains(got, "pkg.go.dev") {
			t.Errorf("%s dropped: %q", link, got)
		}
	}
}
